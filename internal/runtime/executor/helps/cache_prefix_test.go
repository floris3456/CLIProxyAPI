package helps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// All keys and values here are dummies. The fixed test key makes tags deterministic.
const testPrefixKey = "0123456789abcdef0123456789abcdef"
const otherPrefixKey = "fedcba9876543210fedcba9876543210"

var hexTag = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Building blocks for wire bodies. Each is one JSON value so tests can edit one block at a time.
var (
	tAlpha  = `{"name":"alpha","input_schema":{"type":"object"}}`
	tBeta   = `{"name":"beta","input_schema":{"type":"object"}}`
	sA      = `{"type":"text","text":"sys-a"}`
	sB      = `{"type":"text","text":"sys-b"}`
	mHello  = `{"role":"user","content":[{"type":"text","text":"hello"}]}`
	mHi     = `{"role":"assistant","content":[{"type":"text","text":"hi"}]}`
	textSec = `{"type":"text","text":"second"}`
	tPing   = `{"type":"text","text":"ping"}`
	ephem   = `{"type":"ephemeral"}`
)

// wire assembles a Messages body with the given top-level extras (each starting with a comma).
func wire(tools, system, messages []string, extras string) string {
	return `{"model":"claude-sonnet-4-6","max_tokens":16,"tools":[` + strings.Join(tools, ",") +
		`],"system":[` + strings.Join(system, ",") + `],"messages":[` + strings.Join(messages, ",") + `]` + extras + `}`
}

// mark inserts a cache_control member into a JSON object block.
func mark(block, marker string) string {
	return block[:len(block)-1] + `,"cache_control":` + marker + `}`
}

func baseMessages() []string {
	return []string{mHello, mHi, `{"role":"user","content":[` + mark(textSec, ephem) + `]}`}
}

func baseBody() string {
	return wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), "")
}

func testHeader(beta string) http.Header {
	h := http.Header{}
	h.Set("Anthropic-Beta", beta)
	h.Set("Anthropic-Version", "2023-06-01")
	return h
}

func snapWithKey(key, body string, h http.Header) *pluginapi.CachePrefixSnapshot {
	return claudePrefixSnapshotWithKey([]byte(key), []byte(body), h)
}

func snap(body string) *pluginapi.CachePrefixSnapshot {
	return snapWithKey(testPrefixKey, body, testHeader(""))
}

// checkEndpoints asserts the full ordered endpoint list, not just one element.
func checkEndpoints(s *pluginapi.CachePrefixSnapshot, want []pluginapi.CachePrefixEndpoint) error {
	if s == nil {
		return fmt.Errorf("nil snapshot")
	}
	if len(s.Endpoints) != len(want) {
		return fmt.Errorf("endpoints = %+v, want %d entries", s.Endpoints, len(want))
	}
	for i, e := range s.Endpoints {
		w := want[i]
		if e.Ordinal != i || e.Section != w.Section || e.TTL != w.TTL || !hexTag.MatchString(e.Tag) {
			return fmt.Errorf("endpoint[%d] = %+v, want ordinal %d section %s ttl %s", i, e, i, w.Section, w.TTL)
		}
	}
	return nil
}

// lastOnly is a mutation: it keeps only the final endpoint, the bug that checkEndpoints must catch.
func lastOnly(s *pluginapi.CachePrefixSnapshot) *pluginapi.CachePrefixSnapshot {
	out := *s
	out.Endpoints = s.Endpoints[len(s.Endpoints)-1:]
	return &out
}

func TestPrefixSingleMarkerSectionOrdinalTTL(t *testing.T) {
	for _, ttl := range []string{"5m", "1h"} {
		marker := ephem
		if ttl == "1h" {
			marker = `{"type":"ephemeral","ttl":"1h"}`
		}
		body := wire([]string{tAlpha, tBeta}, []string{sA, sB}, []string{mHello, mHi, `{"role":"user","content":[` + mark(textSec, marker) + `]}`}, "")
		s := snap(body)
		if err := checkEndpoints(s, []pluginapi.CachePrefixEndpoint{{Section: "messages", TTL: ttl}}); err != nil {
			t.Fatalf("ttl %s: %v", ttl, err)
		}
		if s.Shape != "text_single_pass" || len(s.Reasons) != 0 || s.Automatic {
			t.Fatalf("ttl %s: shape=%s reasons=%v automatic=%v", ttl, s.Shape, s.Reasons, s.Automatic)
		}
	}
}

func TestPrefixThreeMarkersInWireOrder(t *testing.T) {
	tools := []string{tAlpha, mark(tBeta, ephem)}
	system := []string{sA, mark(sB, `{"type":"ephemeral","ttl":"1h"}`)}
	msgs := []string{mHello, mHi, `{"role":"user","content":[` + mark(textSec, ephem) + `]}`}
	s := snap(wire(tools, system, msgs, ""))
	want := []pluginapi.CachePrefixEndpoint{
		{Section: "tools", TTL: "5m"},
		{Section: "system", TTL: "1h"},
		{Section: "messages", TTL: "5m"},
	}
	if err := checkEndpoints(s, want); err != nil {
		t.Fatal(err)
	}
	// The mutation "only the last marker is an endpoint" must fail the same check.
	if err := checkEndpoints(lastOnly(s), want); err == nil {
		t.Fatal("last-marker-only mutant passed the full ordered check")
	}
}

func TestPrefixTagIsPrefixProperty(t *testing.T) {
	tagOf := func(body string) string {
		t.Helper()
		s := snap(body)
		if len(s.Endpoints) != 1 {
			t.Fatalf("endpoints = %+v", s.Endpoints)
		}
		return s.Endpoints[0].Tag
	}
	base := tagOf(baseBody())
	// Unchanged prefix: later messages and later blocks inside the marked message.
	laterMessage := wire([]string{tAlpha, tBeta}, []string{sA, sB}, append(baseMessages(), mHi), "")
	laterBlock := wire([]string{tAlpha, tBeta}, []string{sA, sB}, []string{mHello, mHi,
		`{"role":"user","content":[` + mark(textSec, ephem) + `,{"type":"text","text":"after"}]}`}, "")
	for name, body := range map[string]string{"later message": laterMessage, "block after marker": laterBlock} {
		if got := tagOf(body); got != base {
			t.Fatalf("%s changed the tag", name)
		}
	}
	// Any byte before or at the marked block changes the tag.
	changed := map[string]string{
		"tool order":      wire([]string{tBeta, tAlpha}, []string{sA, sB}, baseMessages(), ""),
		"system text":     wire([]string{tAlpha, tBeta}, []string{strings.Replace(sA, "sys-a", "sys-x", 1), sB}, baseMessages(), ""),
		"earlier message": wire([]string{tAlpha, tBeta}, []string{sA, sB}, []string{mHello, strings.Replace(mHi, "hi", "ho", 1), baseMessages()[2]}, ""),
		"marked text":     wire([]string{tAlpha, tBeta}, []string{sA, sB}, []string{mHello, mHi, `{"role":"user","content":[` + mark(strings.Replace(textSec, "second", "third", 1), ephem) + `]}`}, ""),
		"marker ttl":      wire([]string{tAlpha, tBeta}, []string{sA, sB}, []string{mHello, mHi, `{"role":"user","content":[` + mark(textSec, `{"type":"ephemeral","ttl":"1h"}`) + `]}`}, ""),
		"image before":    wire([]string{tAlpha, tBeta}, []string{sA, sB}, []string{`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}`, mHi, baseMessages()[2]}, ""),
	}
	for name, body := range changed {
		if got := tagOf(body); got == base {
			t.Fatalf("%s did not change the tag", name)
		}
	}
	// A marker on a later ping block is a different endpoint from the shared block.
	ping := wire([]string{tAlpha, tBeta}, []string{sA, sB}, []string{mHello, mHi, textUser(textSec), textUser(mark(tPing, ephem))}, "")
	if got := tagOf(ping); got == base {
		t.Fatal("ping marker reused the shared-block tag")
	}
}

// textUser wraps a text block as an unmarked user message, keeping the shared history identical.
func textUser(block string) string { return `{"role":"user","content":[` + block + `]}` }

// billingBlock is a cloaked system[0] billing block with the given body after the prefix.
func billingBlock(fields string) string {
	return `{"type":"text","text":"x-anthropic-billing-header: ` + fields + `"}`
}

const billingFields = "cc_version=2.1.280.afb; cc_entrypoint=cli; cch=6a61c; cc_prompt_id=a6d80514-f8de-4941-a7cf-2897a2a3518a; cc_turn_origin=human;"

// The five cch digits of the system[0] billing block are the one audited exclusion (W billing-header audit,
// 2026-10-08): a real pair that differed there shared its whole cached prefix. Every other byte stays in the tag.
func TestPrefixBillingCCHIsTheOnlyAuditedExclusion(t *testing.T) {
	bodyWith := func(system0, system1 string) map[string]string {
		sys := []string{system0, mark(system1, ephem)}
		return map[string]string{
			"system marker":   wire(nil, sys, []string{mHello}, ""),
			"messages marker": wire(nil, []string{system0, system1}, baseMessages(), ""),
		}
	}
	tags := func(system0, system1 string) map[string]string {
		t.Helper()
		out := map[string]string{}
		for name, body := range bodyWith(system0, system1) {
			s := snap(body)
			if len(s.Endpoints) != 1 {
				t.Fatalf("%s: endpoints = %+v", name, s.Endpoints)
			}
			out[name] = s.Endpoints[0].Tag
		}
		return out
	}
	base := tags(billingBlock(billingFields), sA)

	// Equal: only the cch digits differ, in either marker section.
	for name, tag := range tags(billingBlock(strings.Replace(billingFields, "cch=6a61c;", "cch=76e6f;", 1)), sA) {
		if tag != base[name] {
			t.Fatalf("%s: cch-only difference changed the tag", name)
		}
	}

	// Different: every other change, and every cch outside the audited shape.
	changed := map[string][2]string{
		"cc_prompt_id":            {billingBlock(strings.Replace(billingFields, "a6d80514", "b6d80514", 1)), sA},
		"cc_prev_req added":       {billingBlock(strings.Replace(billingFields, "cch=6a61c;", "cch=6a61c; cc_prev_req=req_x;", 1)), sA},
		"build hash":              {billingBlock(strings.Replace(billingFields, "280.afb", "280.afc", 1)), sA},
		"entrypoint":              {billingBlock(strings.Replace(billingFields, "entrypoint=cli", "entrypoint=sdk-cli", 1)), sA},
		"cch removed":             {billingBlock(strings.Replace(billingFields, " cch=6a61c;", "", 1)), sA},
		"uppercase cch":           {billingBlock(strings.Replace(billingFields, "cch=6a61c;", "cch=6A61C;", 1)), sA},
		"six-digit cch":           {billingBlock(strings.Replace(billingFields, "cch=6a61c;", "cch=6a61c0;", 1)), sA},
		"second cch":              {billingBlock(billingFields + " cch=12345;"), sA},
		"later system block text": {billingBlock(billingFields), strings.Replace(sA, "sys-a", "sys-a cch=6a61c;", 1)},
		"not a billing block":     {`{"type":"text","text":"x-other-header: cch=76e6f;"}`, sA},
	}
	for name, blocks := range changed {
		for section, tag := range tags(blocks[0], blocks[1]) {
			if tag == base[section] {
				t.Fatalf("%s (%s) did not change the tag", name, section)
			}
		}
	}

	// Pairs outside the audited shape stay distinct when only their cch digits differ.
	for name, pair := range map[string][2]string{
		"cch in a later system block": {
			strings.Replace(sA, "sys-a", "note cch=6a61c;", 1), strings.Replace(sA, "sys-a", "note cch=76e6f;", 1)},
		"second cch in the billing block": {
			billingBlock(billingFields + " cch=11111;"), billingBlock(billingFields + " cch=22222;")},
		"billing block not at system[0]": {
			billingBlock(billingFields), billingBlock(strings.Replace(billingFields, "cch=6a61c;", "cch=76e6f;", 1))},
	} {
		var first, second map[string]string
		if name == "cch in a later system block" {
			first, second = tags(billingBlock(billingFields), pair[0]), tags(billingBlock(billingFields), pair[1])
		} else if name == "billing block not at system[0]" {
			first, second = tags(sB, pair[0]), tags(sB, pair[1])
		} else {
			first, second = tags(pair[0], sA), tags(pair[1], sA)
		}
		for section := range first {
			if first[section] == second[section] {
				t.Fatalf("%s (%s): cch outside the audited shape was excluded", name, section)
			}
		}
	}

	// The audit covered an unmarked billing block only: a marked one keeps its cch in the tag.
	markedTag := func(fields string) string {
		s := snap(wire(nil, []string{mark(billingBlock(fields), ephem), sA}, []string{mHello}, ""))
		if len(s.Endpoints) != 1 {
			t.Fatalf("marked billing block: endpoints = %+v", s.Endpoints)
		}
		return s.Endpoints[0].Tag
	}
	if markedTag(billingFields) == markedTag(strings.Replace(billingFields, "cch=6a61c;", "cch=76e6f;", 1)) {
		t.Fatal("marked billing block: cch was excluded")
	}

	// The snapshot must not change the body it fingerprints.
	body := bodyWith(billingBlock(billingFields), sA)["system marker"]
	before := body
	snap(body)
	if body != before || !strings.Contains(body, "cch=6a61c;") {
		t.Fatal("snapshot mutated the body")
	}
}

func TestPrefixSettingsTagSensitivity(t *testing.T) {
	base := baseBody()
	baseSettings := snap(base).SettingsTag
	inBody := map[string]string{
		"model":         strings.Replace(base, "claude-sonnet-4-6", "claude-opus-5-5", 1),
		"tool_choice":   wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"tool_choice":{"type":"auto"}`),
		"thinking":      wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"thinking":{"type":"enabled","budget_tokens":1024}`),
		"output_config": wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"output_config":{"effort":"high"}`),
		"unknown key":   wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"x_future_setting":true`),
	}
	for name, body := range inBody {
		if snap(body).SettingsTag == baseSettings {
			t.Fatalf("%s did not change the settings tag", name)
		}
	}
	if snapWithKey(testPrefixKey, base, testHeader("context-1m-2025-08-07")).SettingsTag == baseSettings {
		t.Fatal("anthropic-beta did not change the settings tag")
	}
	ignored := map[string]string{
		"max_tokens":     strings.Replace(base, `"max_tokens":16`, `"max_tokens":4096`, 1),
		"stream":         wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"stream":true`),
		"temperature":    wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"temperature":0.5`),
		"top_p":          wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"top_p":0.9`),
		"top_k":          wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"top_k":5`),
		"stop_sequences": wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"stop_sequences":["END"]`),
		"metadata":       wire([]string{tAlpha, tBeta}, []string{sA, sB}, baseMessages(), `,"metadata":{"user_id":"dummy-user"}`),
	}
	for name, body := range ignored {
		if snap(body).SettingsTag != baseSettings {
			t.Fatalf("%s changed the settings tag", name)
		}
	}
}

func TestPrefixReasonCodes(t *testing.T) {
	marked := baseMessages()
	cases := []struct {
		name   string
		body   string
		reason string
	}{
		{"automatic_marker", wire([]string{tAlpha}, []string{sA}, marked, `,"cache_control":{"type":"ephemeral"}`), "automatic_marker"},
		{"typed_tool", wire([]string{`{"type":"bash_20250124","name":"bash"}`}, []string{sA}, marked, ""), "typed_tool"},
		{"hidden_processing mcp", wire([]string{tAlpha}, []string{sA}, marked, `,"mcp_servers":[]`), "hidden_processing"},
		{"hidden_processing container", wire([]string{tAlpha}, []string{sA}, marked, `,"container":"dummy"`), "hidden_processing"},
		{"hidden_processing context_management", wire([]string{tAlpha}, []string{sA}, marked, `,"context_management":{}`), "hidden_processing"},
		{"thinking enabled", wire([]string{tAlpha}, []string{sA}, marked, `,"thinking":{"type":"enabled","budget_tokens":1024}`), "thinking"},
		{"thinking block", wire([]string{tAlpha}, []string{sA}, []string{mHello, `{"role":"assistant","content":[{"type":"thinking","thinking":"dummy","signature":"dummy"}]}`, textUser(mark(tPing, ephem))}, ""), "thinking"},
		{"non_text_content image", wire([]string{tAlpha}, []string{sA}, []string{`{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}`, mHi, textUser(mark(tPing, ephem))}, ""), "non_text_content"},
		{"non_text_content in tool_result", wire([]string{tAlpha}, []string{sA}, []string{mHello, `{"role":"user","content":[{"type":"tool_result","tool_use_id":"dummy","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`, textUser(mark(tPing, ephem))}, ""), "non_text_content"},
		{"server_tool_use", wire([]string{tAlpha}, []string{sA}, []string{mHello, `{"role":"assistant","content":[{"type":"server_tool_use","id":"dummy","name":"web_search","input":{}}]}`, textUser(mark(tPing, ephem))}, ""), "server_tool"},
		{"server_tool result", wire([]string{tAlpha}, []string{sA}, []string{mHello, `{"role":"assistant","content":[{"type":"web_search_tool_result","tool_use_id":"dummy","content":[]}]}`, textUser(mark(tPing, ephem))}, ""), "server_tool"},
		{"no_endpoint", baseBodyWithoutMarkers(), "no_endpoint"},
		{"invalid_marker", wire([]string{tAlpha}, []string{sA}, []string{mHello, mHi, `{"role":"user","content":[` + mark(textSec, `{"type":"persistent"}`) + `]}`}, ""), "invalid_marker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := snap(tc.body)
			if !prefixHasReason(s.Reasons, tc.reason) {
				t.Fatalf("reasons = %v, want %s", s.Reasons, tc.reason)
			}
			if s.Shape != "unsupported" {
				t.Fatalf("shape = %s, want unsupported", s.Shape)
			}
		})
	}

	t.Run("automatic flag", func(t *testing.T) {
		s := snap(wire([]string{tAlpha}, []string{sA}, marked, `,"cache_control":{"type":"ephemeral"}`))
		if !s.Automatic {
			t.Fatal("top-level cache_control did not set Automatic")
		}
		if len(s.Endpoints) != 1 || s.Endpoints[0].Section != "messages" {
			t.Fatalf("top-level cache_control must not be an endpoint: %+v", s.Endpoints)
		}
	})

	t.Run("invalid marker pairs invalid_marker with no_endpoint and empty endpoints", func(t *testing.T) {
		s := snap(wire([]string{tAlpha}, []string{sA}, []string{mHello, mHi, `{"role":"user","content":[` + mark(textSec, `{"type":"persistent"}`) + `]}`}, ""))
		if !prefixHasReason(s.Reasons, "invalid_marker") || !prefixHasReason(s.Reasons, "no_endpoint") {
			t.Fatalf("reasons = %v, want invalid_marker and no_endpoint", s.Reasons)
		}
		if s.Endpoints == nil || len(s.Endpoints) != 0 {
			t.Fatalf("endpoints = %#v, want empty non-nil", s.Endpoints)
		}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"endpoints":[]`) {
			t.Fatalf("endpoints not []: %s", raw)
		}
	})

	t.Run("zero markers serialize endpoints as []", func(t *testing.T) {
		s := snap(baseBodyWithoutMarkers())
		if len(s.Reasons) != 1 || s.Reasons[0] != "no_endpoint" || s.Endpoints == nil {
			t.Fatalf("snapshot = %+v", s)
		}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"endpoints":[]`) {
			t.Fatalf("endpoints not []: %s", raw)
		}
	})

	t.Run("reasons sorted and deduplicated", func(t *testing.T) {
		body := wire([]string{`{"type":"bash_20250124","name":"bash"}`}, []string{sA}, []string{mHello, `{"role":"assistant","content":[{"type":"thinking","thinking":"dummy","signature":"dummy"}]}`}, `,"thinking":{"type":"enabled"}`)
		s := snap(body)
		if !sort.StringsAreSorted(s.Reasons) {
			t.Fatalf("reasons not sorted: %v", s.Reasons)
		}
		for i := 1; i < len(s.Reasons); i++ {
			if s.Reasons[i] == s.Reasons[i-1] {
				t.Fatalf("reasons duplicated: %v", s.Reasons)
			}
		}
		if !prefixHasReason(s.Reasons, "thinking") {
			t.Fatalf("reasons = %v", s.Reasons)
		}
	})
}

func TestPrefixPrivacyAndKeying(t *testing.T) {
	secrets := []string{"DUMMY_TOOL_NAME_7f3", "DUMMY_TEXT_PAYLOAD_91", "https://dummy.example.invalid/DUMMY_URL_55", "DUMMY_HEADER_VALUE_23", "sk-ant-api-DUMMY_KEY_88"}
	body := wire(
		[]string{fmt.Sprintf(`{"name":%q,"input_schema":{"type":"object"}}`, secrets[0])},
		[]string{fmt.Sprintf(`{"type":"text","text":%q}`, secrets[1])},
		[]string{fmt.Sprintf(`{"role":"user","content":[{"type":"image","source":{"type":"url","url":%q}},%s]}`, secrets[2], mark(`{"type":"text","text":"tail"}`, ephem))},
		`,"metadata":{"user_id":"DUMMY_KEY_88"}`)
	h := testHeader(secrets[3])
	h.Set("X-Api-Key", secrets[4]) // never read by the snapshot; must not leak through it
	s := snapWithKey(testPrefixKey, body, h)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("snapshot leaked %q: %s", secret, raw)
		}
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		switch key {
		case "version", "key_id", "settings_tag", "endpoints", "automatic", "shape", "reasons":
		default:
			t.Fatalf("unexpected snapshot field %q", key)
		}
	}
	if !hexTag.MatchString(s.SettingsTag) || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(s.KeyID) {
		t.Fatalf("bad key_id/settings_tag: %s %s", s.KeyID, s.SettingsTag)
	}

	again := snapWithKey(testPrefixKey, body, h)
	if fmt.Sprint(again) != fmt.Sprint(s) {
		t.Fatal("same key is not deterministic")
	}
	other := snapWithKey(otherPrefixKey, body, h)
	if other.KeyID == s.KeyID || other.SettingsTag == s.SettingsTag || other.Endpoints[0].Tag == s.Endpoints[0].Tag {
		t.Fatal("different keys produced comparable tags")
	}
}

func TestPrefixSnapshotDoesNotMutateBodyOrHeaders(t *testing.T) {
	body := []byte(baseBody())
	h := testHeader("beta-a,beta-b")
	h.Set("X-Pacer-Cache-Capability", "dummy")
	beforeBody := append([]byte(nil), body...)
	beforeHeader := h.Clone()
	claudePrefixSnapshot(body, h)
	if string(body) != string(beforeBody) {
		t.Fatal("snapshot mutated the body")
	}
	if fmt.Sprint(h) != fmt.Sprint(beforeHeader) {
		t.Fatal("snapshot mutated headers")
	}
}

func TestCloneEvidenceDeepCopiesPrefix(t *testing.T) {
	orig := &pluginapi.CachePolicyEvidence{Prefix: &pluginapi.CachePrefixSnapshot{
		Version:   1,
		Endpoints: []pluginapi.CachePrefixEndpoint{{Ordinal: 0, Section: "messages", TTL: "5m", Tag: strings.Repeat("a", 64)}},
		Reasons:   []string{"thinking"},
	}}
	cp := cachepolicy.CloneEvidence(orig)
	cp.Prefix.Endpoints[0].Tag = strings.Repeat("b", 64)
	cp.Prefix.Reasons[0] = "typed_tool"
	cp.Prefix.Endpoints = append(cp.Prefix.Endpoints, pluginapi.CachePrefixEndpoint{Ordinal: 1})
	if orig.Prefix.Endpoints[0].Tag != strings.Repeat("a", 64) || orig.Prefix.Reasons[0] != "thinking" || len(orig.Prefix.Endpoints) != 1 {
		t.Fatalf("CloneEvidence shares prefix state: %+v", orig.Prefix)
	}
}

func baseBodyWithoutMarkers() string {
	return wire([]string{tAlpha, tBeta}, []string{sA, sB}, []string{mHello, mHi, textUser(textSec)}, "")
}

func prefixHasReason(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
