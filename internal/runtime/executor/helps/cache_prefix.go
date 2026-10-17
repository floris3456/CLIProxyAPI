package helps

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

// This file builds CachePolicyEvidence.Prefix (contract §S1). The snapshot is content-free:
// every content-bearing value enters only through a keyed HMAC, and the key never leaves
// the process. Tags from different keys are incomparable, which is the intended discontinuity
// after a restart.

const (
	prefixSnapshotVersion  = 1
	prefixKeyIDDomain      = "pacer-prefix-key-id-v1\x00"
	prefixEndpointDomain   = "pacer-prefix-v1\x00"
	prefixSettingsDomain   = "pacer-settings-v1\x00"
	prefixShapeSingle      = "text_single_pass"
	prefixShapeUnsupported = "unsupported"
)

// Reason codes are a fixed set shared with the Pacer validator. Do not add free-form text.
const (
	reasonAutomaticMarker  = "automatic_marker"
	reasonTypedTool        = "typed_tool"
	reasonHiddenProcessing = "hidden_processing"
	reasonThinking         = "thinking"
	reasonNonTextContent   = "non_text_content"
	reasonServerTool       = "server_tool"
	reasonNoEndpoint       = "no_endpoint"
	reasonInvalidMarker    = "invalid_marker"
)

// prefixSettingsIgnored lists top-level keys whose values do not change what the upstream
// caches. Everything else, including unknown future keys, enters the settings tag.
var prefixSettingsIgnored = map[string]bool{
	"tools": true, "system": true, "messages": true,
	"max_tokens": true, "stream": true, "temperature": true, "top_p": true, "top_k": true,
	"stop_sequences": true, "metadata": true,
}

var (
	prefixKeyOnce sync.Once
	prefixKey     []byte
)

// claudePrefixSnapshot fingerprints the final signed body and final headers under the process key.
// It must run after the beta pairing and must not mutate its arguments. It returns nil only if the
// OS random source fails; the evidence is optional, so dispatch still proceeds.
func claudePrefixSnapshot(body []byte, header http.Header) *pluginapi.CachePrefixSnapshot {
	prefixKeyOnce.Do(func() {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err == nil {
			prefixKey = key
		}
	})
	if prefixKey == nil {
		return nil
	}
	return claudePrefixSnapshotWithKey(prefixKey, body, header)
}

// claudePrefixSnapshotWithKey is the deterministic core. Tests inject a dummy key through it.
func claudePrefixSnapshotWithKey(key, body []byte, header http.Header) *pluginapi.CachePrefixSnapshot {
	keyID := sha256.Sum256(append([]byte(prefixKeyIDDomain), key...))
	s := &pluginapi.CachePrefixSnapshot{
		Version:     prefixSnapshotVersion,
		KeyID:       hex.EncodeToString(keyID[:8]),
		SettingsTag: prefixSettingsTag(key, body, header),
		Endpoints:   []pluginapi.CachePrefixEndpoint{},
	}

	reasons := map[string]bool{}
	if gjson.GetBytes(body, "cache_control").Exists() {
		// Top-level cache_control is automatic caching, not an explicit endpoint.
		s.Automatic = true
		reasons[reasonAutomaticMarker] = true
	}
	collectStaticPrefixReasons(body, reasons)

	// claudeCacheMarkerPaths reports top-level cache_control too; drop it to get explicit markers.
	paths, err := claudeCacheMarkerPaths(body)
	var explicit []string
	for _, path := range paths {
		if path != "cache_control" {
			explicit = append(explicit, path)
		}
	}
	switch {
	case err != nil:
		// Marker placement is unknowable, so no endpoint is emitted. Empty endpoints always carry
		// no_endpoint as well: the Pacer validator relies on that pairing.
		reasons[reasonInvalidMarker] = true
		reasons[reasonNoEndpoint] = true
	case len(explicit) == 0:
		reasons[reasonNoEndpoint] = true
	default:
		endpoints := make([]pluginapi.CachePrefixEndpoint, 0, len(explicit))
		for ordinal, path := range explicit {
			m, ok := parsePrefixMarker(path)
			if !ok {
				// Unparseable path: drop every endpoint and say so with the same pair of codes.
				reasons[reasonInvalidMarker] = true
				reasons[reasonNoEndpoint] = true
				endpoints = endpoints[:0]
				break
			}
			// An absent ttl is the wire default. The value is otherwise reported verbatim.
			ttl := gjson.GetBytes(body, path+".ttl").String()
			if ttl == "" {
				ttl = "5m"
			}
			endpoints = append(endpoints, pluginapi.CachePrefixEndpoint{
				Ordinal: ordinal,
				Section: m.section,
				TTL:     ttl,
				Tag:     prefixEndpointTag(key, body, m),
			})
		}
		s.Endpoints = append(s.Endpoints, endpoints...)
	}

	s.Reasons = sortedPrefixReasons(reasons)
	s.Shape = prefixShapeSingle
	if len(s.Reasons) > 0 {
		s.Shape = prefixShapeUnsupported
	}
	return s
}

// collectStaticPrefixReasons scans for reasons that do not depend on marker placement.
func collectStaticPrefixReasons(body []byte, reasons map[string]bool) {
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if typ := tool.Get("type"); typ.Exists() && typ.String() != "custom" {
			reasons[reasonTypedTool] = true
		}
	}
	for _, key := range []string{"mcp_servers", "container", "context_management"} {
		if gjson.GetBytes(body, key).Exists() {
			reasons[reasonHiddenProcessing] = true
		}
	}
	if typ := gjson.GetBytes(body, "thinking.type"); typ.Exists() && typ.String() != "disabled" {
		reasons[reasonThinking] = true
	}
	if system := gjson.GetBytes(body, "system"); system.IsArray() {
		for _, block := range system.Array() {
			scanPrefixBlock(block, reasons)
		}
	}
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		if content := message.Get("content"); content.IsArray() {
			for _, block := range content.Array() {
				scanPrefixBlock(block, reasons)
			}
		}
	}
}

func scanPrefixBlock(block gjson.Result, reasons map[string]bool) {
	typ := block.Get("type").String()
	if typ == "thinking" || typ == "redacted_thinking" {
		reasons[reasonThinking] = true
	}
	if typ == "server_tool_use" || (strings.HasSuffix(typ, "_tool_result") && typ != "tool_result") {
		reasons[reasonServerTool] = true
	}
	switch typ {
	case "text", "tool_use":
	case "tool_result":
		// String tool_result content is text. Array content must hold only text blocks.
		if content := block.Get("content"); content.IsArray() {
			for _, inner := range content.Array() {
				if inner.Get("type").String() != "text" {
					reasons[reasonNonTextContent] = true
				}
			}
		}
	default:
		reasons[reasonNonTextContent] = true
	}
}

// prefixMarker locates one explicit marker in the final body.
type prefixMarker struct {
	section string // tools | system | messages
	index   int    // tool index, system block index, or message index
	block   int    // content block index inside the message (messages only)
}

// parsePrefixMarker decodes a path returned by claudeCacheMarkerPaths, for example
// "tools.0.cache_control", "system.1.cache_control" or "messages.2.content.0.cache_control".
func parsePrefixMarker(path string) (prefixMarker, bool) {
	parts := strings.Split(path, ".")
	switch {
	case len(parts) == 3 && (parts[0] == "tools" || parts[0] == "system") && parts[2] == "cache_control":
		i, err := strconv.Atoi(parts[1])
		return prefixMarker{section: parts[0], index: i}, err == nil
	case len(parts) == 5 && parts[0] == "messages" && parts[2] == "content" && parts[4] == "cache_control":
		i, errIndex := strconv.Atoi(parts[1])
		j, errBlock := strconv.Atoi(parts[3])
		return prefixMarker{section: "messages", index: i, block: j}, errIndex == nil && errBlock == nil
	}
	return prefixMarker{}, false
}

// prefixWriter streams length-prefixed records into an HMAC. Each record is one kind byte,
// an 8-byte big-endian length and the raw bytes, so record boundaries cannot be forged.
type prefixWriter struct{ mac hash.Hash }

func newPrefixWriter(key []byte, domain string) prefixWriter {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	return prefixWriter{mac: mac}
}

func (w prefixWriter) record(kind byte, raw string) {
	var head [9]byte
	head[0] = kind
	binary.BigEndian.PutUint64(head[1:], uint64(len(raw)))
	w.mac.Write(head[:])
	w.mac.Write([]byte(raw))
}

func (w prefixWriter) sum() string { return hex.EncodeToString(w.mac.Sum(nil)) }

// prefixEndpointTag is the cumulative final-wire prefix through the marked block. Records are
// raw gjson bytes with no re-encoding, so the tag changes with any byte at or before the marked
// block and ignores everything after it, except the audited billing cch digits (prefixSystemBlockRecord).
// Order: tools, system, messages.
func prefixEndpointTag(key, body []byte, m prefixMarker) string {
	w := newPrefixWriter(key, prefixEndpointDomain)

	// Tools up to and including the marked tool. Past the tools section, every tool is prefix.
	tools := gjson.GetBytes(body, "tools").Array()
	n := len(tools)
	if m.section == "tools" && m.index+1 < n {
		n = m.index + 1
	}
	for _, tool := range tools[:n] {
		w.record('T', tool.Raw)
	}

	// System is prefix whenever the marker is past the tools section.
	if m.section != "tools" {
		system := gjson.GetBytes(body, "system")
		switch {
		case system.IsArray():
			blocks := system.Array()
			n = len(blocks)
			if m.section == "system" && m.index+1 < n {
				n = m.index + 1
			}
			for i, block := range blocks[:n] {
				w.record('S', prefixSystemBlockRecord(i, block))
			}
		case system.Exists() && system.Type != gjson.Null:
			w.record('S', system.Raw)
		}
	}

	// Messages are prefix only when the marker is inside messages.
	if m.section == "messages" {
		for i, message := range gjson.GetBytes(body, "messages").Array() {
			if i > m.index {
				break
			}
			w.record('M', message.Get("role").Raw)
			content := message.Get("content")
			if i < m.index {
				writeContentRecords(w, content, -1)
				continue
			}
			writeContentRecords(w, content, m.block)
			break
		}
	}
	return w.sum()
}

const prefixBillingHeaderPrefix = "x-anthropic-billing-header:"

// prefixSystemBlockRecord returns the bytes recorded for system block i: the raw block, with one audited
// exclusion. In an unmarked system[0] text block that starts with the billing header, the first
// `cch=` followed by exactly five lowercase hex digits and `;` is recorded as `cch=00000;`. Those are the
// five bytes CPA's signer zeroes before hashing (claude_signing.go); a real request pair that differed
// only there shared its whole cached prefix (pacer evidence/implementation/billing-header-audit, 2026-10-08).
// Every other byte, including every other billing field, stays in the record. Any doubt keeps the raw block.
func prefixSystemBlockRecord(i int, block gjson.Result) string {
	if i != 0 || block.Get("type").String() != "text" || block.Get("cache_control").Exists() {
		return block.Raw
	}
	text := block.Get("text")
	if text.Type != gjson.String || !strings.HasPrefix(text.String(), prefixBillingHeaderPrefix) {
		return block.Raw
	}
	// Locate the raw text token inside the raw block without re-encoding anything.
	start := text.Index - block.Index
	if start < 0 || start+len(text.Raw) > len(block.Raw) || block.Raw[start:start+len(text.Raw)] != text.Raw {
		if strings.Count(block.Raw, text.Raw) != 1 {
			return block.Raw
		}
		start = strings.Index(block.Raw, text.Raw)
	}
	digits, ok := prefixBillingCCHDigits(text.Raw)
	if !ok {
		return block.Raw
	}
	at := start + digits
	return block.Raw[:at] + "00000" + block.Raw[at+5:]
}

// prefixBillingCCHDigits mirrors claudeBillingCCHDigitsOffset: the offset of the first valid cch digits.
func prefixBillingCCHDigits(raw string) (int, bool) {
	for from := 0; from < len(raw); {
		rel := strings.Index(raw[from:], "cch=")
		if rel < 0 {
			return 0, false
		}
		digits := from + rel + len("cch=")
		end := digits + 5
		if end < len(raw) && raw[end] == ';' && prefixIsLowerHex(raw[digits:end]) {
			return digits, true
		}
		from = digits
	}
	return 0, false
}

func prefixIsLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return len(s) == 5
}

// writeContentRecords emits content as C records. A string is one record. An array is one record
// per block, up to and including limit (limit < 0 means all blocks).
func writeContentRecords(w prefixWriter, content gjson.Result, limit int) {
	if content.IsArray() {
		for j, block := range content.Array() {
			if limit >= 0 && j > limit {
				break
			}
			w.record('C', block.Raw)
		}
		return
	}
	if content.Exists() && content.Type != gjson.Null {
		w.record('C', content.Raw)
	}
}

// prefixSettingsTag covers the request settings that change upstream processing: the beta and
// version headers, then every top-level key not in prefixSettingsIgnored, sorted by key.
func prefixSettingsTag(key, body []byte, header http.Header) string {
	w := newPrefixWriter(key, prefixSettingsDomain)
	w.record('H', strings.Join(header.Values("Anthropic-Beta"), ","))
	w.record('H', strings.Join(header.Values("Anthropic-Version"), ","))

	top := gjson.ParseBytes(body)
	if !top.IsObject() {
		return w.sum()
	}
	type setting struct {
		name  string
		value string
	}
	var settings []setting
	top.ForEach(func(k, v gjson.Result) bool {
		if name := k.String(); !prefixSettingsIgnored[name] {
			settings = append(settings, setting{name: name, value: v.Raw})
		}
		return true
	})
	sort.SliceStable(settings, func(i, j int) bool { return settings[i].name < settings[j].name })
	for _, s := range settings {
		w.record('K', s.name)
		w.record('V', s.value)
	}
	return w.sum()
}

func sortedPrefixReasons(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for reason := range set {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}
