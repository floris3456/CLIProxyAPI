package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Every credential and value in this file is a dummy. The transport is a fake
// api.anthropic.com, so nothing leaves the process.

const prefixWireModel = "claude-sonnet-4-5"

// Shared history fixtures. B is the marked user block; ping is a later user block.
var (
	pwToolAlpha = `{"name":"alpha","input_schema":{"type":"object","properties":{}}}`
	pwToolBeta  = `{"name":"beta","input_schema":{"type":"object","properties":{}}}`
	pwSystemA   = `{"type":"text","text":"dummy system one"}`
	pwSystemB   = `{"type":"text","text":"dummy system two"}`
	pwHello     = `{"role":"user","content":[{"type":"text","text":"dummy hello"}]}`
	pwReply     = `{"role":"assistant","content":[{"type":"text","text":"dummy reply"}]}`
	pwBMarked   = `{"role":"user","content":[{"type":"text","text":"dummy question","cache_control":{"type":"ephemeral"}}]}`
	pwBPlain    = `{"role":"user","content":[{"type":"text","text":"dummy question"}]}`
	pwAnswer    = `{"role":"assistant","content":[{"type":"text","text":"dummy answer"}]}`
	pwPingPlain = `{"role":"user","content":[{"type":"text","text":"dummy ping"}]}`
	pwPingMark  = `{"role":"user","content":[{"type":"text","text":"dummy ping","cache_control":{"type":"ephemeral"}}]}`
	pwImageHi   = `{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"dummy hello"}]}`
	pwEffort    = `,"output_config":{"effort":"medium"}`
)

func pwPayload(tools []string, messages []string, extras string) string {
	return `{"model":"` + prefixWireModel + `","max_tokens":16,"tools":[` + strings.Join(tools, ",") +
		`],"system":[` + pwSystemA + `,` + pwSystemB + `],"messages":[` + strings.Join(messages, ",") + `]` + extras + `}`
}

// primaryPayload ends at B, which carries the only marker.
func primaryPayload() string {
	return pwPayload([]string{pwToolAlpha, pwToolBeta}, []string{pwHello, pwReply, pwBMarked}, pwEffort)
}

// warmingPayload is the same history through B plus a later unmarked ping block.
func warmingPayload() string {
	return pwPayload([]string{pwToolAlpha, pwToolBeta}, []string{pwHello, pwReply, pwBMarked, pwAnswer, pwPingPlain}, pwEffort)
}

type prefixWireSink struct {
	authID  string
	records chan usage.Record
}

func (s *prefixWireSink) HandleUsage(_ context.Context, r usage.Record) {
	if r.AuthID == s.authID {
		s.records <- r
	}
}

type prefixWireCase struct {
	label   string
	stream  bool
	source  sdktranslator.Format
	payload string
	kind    string // primary | warming
	managed bool
	oauth   bool // dummy Claude OAuth token: the cloaked, cch-signed route
}

type prefixWireResult struct {
	record usage.Record
	sent   []byte
	header http.Header
}

// runPrefixWire executes one request through the real executor and returns the usage record
// together with the exact bytes and headers the fake upstream received.
func runPrefixWire(t *testing.T, c prefixWireCase) prefixWireResult {
	t.Helper()
	authID := "pacer-prefix-wire-" + c.label
	sinkName := t.Name() + "/" + c.label
	sink := &prefixWireSink{authID: authID, records: make(chan usage.Record, 1)}
	usage.RegisterNamedPlugin(sinkName, sink)

	a := &auth.Auth{ID: authID, Index: "index-" + c.label, Provider: "claude", Attributes: map[string]string{"api_key": "dummy-prefix-key"}}
	if c.oauth {
		// One credential identity for every oauth case, as for two requests of one real chat.
		a.Attributes["api_key"] = "sk-ant-oat01-dummy-prefix-oauth"
		a.Metadata = map[string]any{"account_uuid": "11111111-2222-4333-8444-555555555555"}
	}
	sentCh := make(chan prefixWireResult, 1)
	transport := claudeSourceTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != "https" || req.URL.Host != "api.anthropic.com" || req.URL.Path != "/v1/messages" {
			return nil, fmt.Errorf("unexpected dispatch %s", req.URL)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		sentCh <- prefixWireResult{sent: body, header: req.Header.Clone()}
		// Reply as a stream when either the client or the upstream body asks for one: Responses
		// clients are translated into an upstream stream even when they did not ask for one.
		if c.stream || gjson.GetBytes(body, "stream").Bool() {
			events := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_dummy\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"" + prefixWireModel + "\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n"
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events)), Request: req}, nil
		}
		payload := `{"id":"msg_dummy","type":"message","role":"assistant","model":"` + prefixWireModel + `","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
	})

	ctx, cancel := context.WithCancel(usage.WithStream(context.Background(), c.stream))
	t.Cleanup(cancel)
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
	opts := coreexecutor.Options{Stream: c.stream, SourceFormat: c.source}
	if c.managed {
		opts.CachePolicy = &pluginapi.CachePolicy{Protocol: 1, Scope: pluginapi.CachePolicyScope{CallerScope: strings.Repeat("a", 64), InstallationID: "dummy-install", SessionID: "session-" + c.label, ProviderRoute: "claude"}, ContextEpoch: "epoch", Revision: 1, TTL: "5m", Kind: c.kind}
		opts.LifecycleRequestID = "dummy-lifecycle-" + c.label
		opts.CachePolicyAttempt = func(_ context.Context, r pluginapi.CachePolicyAttemptRequest) (pluginapi.CachePolicyAttemptResponse, error) {
			return pluginapi.CachePolicyAttemptResponse{Policy: r.Policy, StartedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
		}
	}

	e := NewClaudeExecutor(&config.Config{})
	req := coreexecutor.Request{Model: prefixWireModel, Payload: []byte(c.payload)}
	var err error
	if c.stream {
		var res *coreexecutor.StreamResult
		res, err = e.ExecuteStream(ctx, a, req, opts)
		if err == nil {
			for chunk := range res.Chunks {
				if chunk.Err != nil {
					err = errors.Join(err, chunk.Err)
				}
			}
		}
	} else {
		_, err = e.Execute(ctx, a, req, opts)
	}
	if err != nil {
		t.Fatalf("%s execute: %v", c.label, err)
	}

	var out prefixWireResult
	select {
	case out = <-sentCh:
	default:
		t.Fatalf("%s: transport not reached", c.label)
	}
	select {
	case out.record = <-sink.records:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: usage record not delivered", c.label)
	}
	return out
}

// prefixOf returns the snapshot of a record that must be managed.
func prefixOf(t *testing.T, r usage.Record) *pluginapi.CachePrefixSnapshot {
	t.Helper()
	if r.CachePolicy == nil || r.CachePolicy.Prefix == nil {
		t.Fatalf("usage record lacks CachePolicy.Prefix: %+v", r.CachePolicy)
	}
	return r.CachePolicy.Prefix
}

// warmsPrimary is the relation the Pacer qualifier checks: the warming snapshot has one endpoint
// with the same tag as the primary's endpoint at B, under the same key and settings.
func warmsPrimary(primary, warm *pluginapi.CachePrefixSnapshot) bool {
	if len(primary.Endpoints) != 1 || len(warm.Endpoints) != 1 {
		return false
	}
	return warm.Endpoints[0].Tag == primary.Endpoints[0].Tag && warm.KeyID == primary.KeyID && warm.SettingsTag == primary.SettingsTag
}

func TestPrefixWarmingRequestMatchesPrimaryAtMessagesBoundary(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			source := sdktranslator.FromString("claude")
			primary := runPrefixWire(t, prefixWireCase{label: name + "-primary", stream: stream, source: source, payload: primaryPayload(), kind: "primary", managed: true})
			pSnap := prefixOf(t, primary.record)
			if len(pSnap.Endpoints) != 1 || pSnap.Endpoints[0].Section != "messages" || pSnap.Shape != "text_single_pass" {
				t.Fatalf("primary snapshot = %+v", pSnap)
			}

			warm := runPrefixWire(t, prefixWireCase{label: name + "-warm", stream: stream, source: source, payload: warmingPayload(), kind: "warming", managed: true})
			wSnap := prefixOf(t, warm.record)
			if !warmsPrimary(pSnap, wSnap) {
				t.Fatalf("warming snapshot does not match primary at B:\nprimary=%+v\nwarming=%+v", pSnap, wSnap)
			}

			// Negative controls: each must fail the same relation.
			negatives := map[string]string{
				"marker on ping": pwPayload([]string{pwToolAlpha, pwToolBeta}, []string{pwHello, pwReply, pwBPlain, pwAnswer, pwPingMark}, pwEffort),
				"tool order":     pwPayload([]string{pwToolBeta, pwToolAlpha}, []string{pwHello, pwReply, pwBMarked, pwAnswer, pwPingPlain}, pwEffort),
				"image before B": pwPayload([]string{pwToolAlpha, pwToolBeta}, []string{pwImageHi, pwReply, pwBMarked, pwAnswer, pwPingPlain}, pwEffort),
				"thinking":       pwPayload([]string{pwToolAlpha, pwToolBeta}, []string{pwHello, pwReply, pwBMarked, pwAnswer, pwPingPlain}, pwEffort+`,"thinking":{"type":"enabled","budget_tokens":1024}`),
				"effort high":    pwPayload([]string{pwToolAlpha, pwToolBeta}, []string{pwHello, pwReply, pwBMarked, pwAnswer, pwPingPlain}, `,"output_config":{"effort":"high"}`),
			}
			for label, payload := range negatives {
				n := runPrefixWire(t, prefixWireCase{label: name + "-neg-" + strings.ReplaceAll(label, " ", "-"), stream: stream, source: source, payload: payload, kind: "warming", managed: true})
				if warmsPrimary(pSnap, prefixOf(t, n.record)) {
					t.Fatalf("negative control %q still satisfies the warming relation: %+v", label, prefixOf(t, n.record))
				}
			}
		})
	}
}

// The billing-header audit pair (pacer evidence/implementation/billing-header-audit): A, then its
// continuation B, on the cloaked OAuth route. CPA signs a different cch into system[0] for each body; every
// endpoint they share must still carry the same tag, and the cch bytes must really differ on the wire.
func TestPrefixCloakedBillingCCHKeepsSharedEndpointTags(t *testing.T) {
	text := `{"type":"text","text":"dummy reference text","cache_control":{"type":"ephemeral"}}`
	u1 := `{"role":"user","content":[{"type":"text","text":"dummy request alpha"}]}`
	a := `{"model":"` + prefixWireModel + `","max_tokens":8,"thinking":{"type":"disabled"},"system":[` + text + `],"messages":[` + u1 + `]}`
	b := `{"model":"` + prefixWireModel + `","max_tokens":8,"thinking":{"type":"disabled"},"system":[` + text + `],"messages":[` + u1 +
		`,{"role":"assistant","content":[{"type":"text","text":"OK"}]},{"role":"user","content":[{"type":"text","text":"dummy request bravo"}]}]}`
	source := sdktranslator.FromString("claude")
	ra := runPrefixWire(t, prefixWireCase{label: "cloak-a", source: source, payload: a, kind: "primary", managed: true, oauth: true})
	rb := runPrefixWire(t, prefixWireCase{label: "cloak-b", source: source, payload: b, kind: "warming", managed: true, oauth: true})

	billA, billB := gjson.GetBytes(ra.sent, "system.0.text").String(), gjson.GetBytes(rb.sent, "system.0.text").String()
	cchOf := func(s string) string {
		i := strings.Index(s, " cch=")
		if !strings.HasPrefix(s, "x-anthropic-billing-header:") || i < 0 || len(s) < i+10 {
			t.Fatalf("no signed billing block in system[0]: %q", s)
		}
		return s[i+5 : i+10]
	}
	if cchOf(billA) == cchOf(billB) || cchOf(billA) == "00000" {
		t.Fatalf("fixture did not produce differing signed cch: %q / %q", billA, billB)
	}
	if strings.Replace(billA, cchOf(billA), "", 1) != strings.Replace(billB, cchOf(billB), "", 1) {
		t.Fatalf("billing blocks differ beyond cch, so this is not the audited case: %q / %q", billA, billB)
	}

	sa, sb := prefixOf(t, ra.record), prefixOf(t, rb.record)
	if sa.KeyID != sb.KeyID || sa.SettingsTag != sb.SettingsTag {
		t.Fatalf("key or settings differ: %+v / %+v", sa, sb)
	}
	if len(sa.Endpoints) == 0 || len(sb.Endpoints) <= len(sa.Endpoints) {
		t.Fatalf("unexpected endpoint counts: A=%+v B=%+v", sa.Endpoints, sb.Endpoints)
	}
	for i, ea := range sa.Endpoints {
		if eb := sb.Endpoints[i]; ea.Section != eb.Section || ea.Tag != eb.Tag {
			t.Fatalf("shared endpoint %d differs: A=%+v B=%+v", i, ea, eb)
		}
	}
	if last := sb.Endpoints[len(sb.Endpoints)-1]; last.Tag == sa.Endpoints[len(sa.Endpoints)-1].Tag {
		t.Fatal("B's own later endpoint reused A's tag")
	}
}

// Responses clients reach the same Messages wire through the translator. Part-level cache_control
// on the input item carries the marker to B, the same history as the Messages fixture above.
func responsesPayload(items ...string) string {
	return `{"model":"` + prefixWireModel + `","max_output_tokens":16,"instructions":"dummy system one",` +
		`"tools":[{"type":"function","name":"alpha","parameters":{"type":"object","properties":{}}}],"input":[` + strings.Join(items, ",") + `]}`
}

func TestPrefixWarmingRequestResponsesTransport(t *testing.T) {
	const (
		rHello  = `{"type":"message","role":"user","content":[{"type":"input_text","text":"dummy hello"}]}`
		rReply  = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"dummy reply"}]}`
		rMarked = `{"type":"message","role":"user","content":[{"type":"input_text","text":"dummy question","cache_control":{"type":"ephemeral"}}]}`
		rAnswer = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"dummy answer"}]}`
		rPing   = `{"type":"message","role":"user","content":[{"type":"input_text","text":"dummy ping"}]}`
	)
	source := sdktranslator.FormatOpenAIResponse
	primary := runPrefixWire(t, prefixWireCase{label: "responses-primary", source: source, payload: responsesPayload(rHello, rReply, rMarked), kind: "primary", managed: true})
	warm := runPrefixWire(t, prefixWireCase{label: "responses-warm", source: source, payload: responsesPayload(rHello, rReply, rMarked, rAnswer, rPing), kind: "warming", managed: true})
	pSnap, wSnap := prefixOf(t, primary.record), prefixOf(t, warm.record)
	t.Logf("responses primary snapshot: endpoints=%+v reasons=%v", pSnap.Endpoints, pSnap.Reasons)
	t.Logf("responses warming snapshot: endpoints=%+v reasons=%v", wSnap.Endpoints, wSnap.Reasons)
	// Outcome is named explicitly: the Responses translator keeps the single marker on B, so the
	// relation holds. If that ever changes, this fails and names the outcome instead of passing silently.
	if !warmsPrimary(pSnap, wSnap) {
		t.Fatalf("responses outcome = RELATION FAILS (marker not placed on B in final wire); primary endpoints=%+v warming endpoints=%+v", pSnap.Endpoints, wSnap.Endpoints)
	}
	t.Log("responses outcome = RELATION HOLDS: single marker on B in both final wires; same tag, key_id and settings_tag")
}

// The helps unit test proves the pure function; this proves the production call site uses the
// same final body. Anything that changes bytes before the snapshot would break warmsPrimary.
func TestPrefixSnapshotMatchesFinalWireBytes(t *testing.T) {
	source := sdktranslator.FromString("claude")
	// Managed enforcement writes ttl onto each marker. An explicit 5m on both sides keeps the
	// comparison about the snapshot alone, not about the existing TTL enforcement.
	payload := pwPayload([]string{pwToolAlpha, pwToolBeta}, []string{pwHello, pwReply, strings.Replace(pwBMarked, `"ephemeral"}`, `"ephemeral","ttl":"5m"}`, 1)}, pwEffort)
	managed := runPrefixWire(t, prefixWireCase{label: "bytes-managed", source: source, payload: payload, kind: "primary", managed: true})
	plain := runPrefixWire(t, prefixWireCase{label: "bytes-plain", source: source, payload: payload, managed: false})
	if !bytes.Equal(managed.sent, plain.sent) {
		t.Fatalf("managed evidence changed upstream bytes:\nmanaged=%s\nplain=%s", managed.sent, plain.sent)
	}
	keys := func(h http.Header) []string {
		var out []string
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "x-pacer-") {
				continue
			}
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if strings.Join(keys(managed.header), ",") != strings.Join(keys(plain.header), ",") {
		t.Fatalf("header set changed: %v vs %v", keys(managed.header), keys(plain.header))
	}
	if got := managed.header.Get("Anthropic-Beta"); got != plain.header.Get("Anthropic-Beta") {
		t.Fatalf("beta header changed: %q vs %q", got, plain.header.Get("Anthropic-Beta"))
	}
	if prefixOf(t, managed.record).Endpoints[0].Tag == "" {
		t.Fatal("managed evidence lost its endpoint")
	}
}
