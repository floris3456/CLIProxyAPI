package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const v8TestBase = "/v8/management/plugins/owned"

type managementV8RPCFixture struct {
	schema       uint32
	registration pluginapi.ManagementRegistrationRequest
	request      pluginapi.ManagementRequest
	routes       []pluginapi.ManagementRoute
	response     pluginapi.ManagementResponse
	err          error
	calls        int
}

func (*managementV8RPCFixture) Shutdown() {}
func (p *managementV8RPCFixture) Call(_ context.Context, method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister:
		return marshalRPCResult(rpcRegistration{SchemaVersion: p.schema, Capabilities: rpcCapabilities{ManagementAPI: true}})
	case pluginabi.MethodManagementRegister:
		if err := json.Unmarshal(raw, &p.registration); err != nil {
			return nil, err
		}
		return marshalRPCResult(rpcManagementRegistrationResponse{Routes: p.routes})
	case pluginabi.MethodManagementHandle:
		p.calls++
		if err := json.Unmarshal(raw, &p.request); err != nil {
			return nil, err
		}
		if p.err != nil {
			return nil, p.err
		}
		return marshalRPCResult(p.response)
	}
	return nil, errors.New("unexpected fixture RPC")
}

func v8RPCHost(t *testing.T, p *managementV8RPCFixture) *Host {
	t.Helper()
	h := New()
	plugin, err := registerRPCPlugin(context.Background(), h, "owned", p, pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.RegisterPluginForTest("owned", plugin)
	h.RegisterManagementRoutes(context.Background(), nil)
	return h
}

func TestManagementV8NativeAdapterAdvertisementAndOpaqueBytes(t *testing.T) {
	body := []byte("{\n\"filename\":\"<a&b>.json\",\"content\":\"{\\\"n\\\":9007199254740993}\\n\",\"value\":1.234567890123456789}")
	p := &managementV8RPCFixture{schema: 7, routes: []pluginapi.ManagementRoute{{Method: "POST", Path: v8TestBase + "/report"}},
		response: pluginapi.ManagementResponse{Body: body, Headers: http.Header{
			"Content-Type": {"application/json; charset=utf-8"}, "Content-Disposition": {`attachment; filename="report.json"`},
			"Set-Cookie": {"private=secret"}, "Authorization": {"Bearer private"}, "Location": {"https://invalid.example/private"},
			"Cache-Control": {"public"}, "X-Pacer-Cache-Capability": {"private"}, "Www-Authenticate": {"private"},
		}}}
	h := v8RPCHost(t, p)
	if p.registration.BasePath != "/v0/management" || p.registration.ResourceBasePath != "/v0/resource/plugins/owned" || p.registration.PluginManagementBasePath != v8TestBase {
		t.Fatalf("registration=%+v", p.registration)
	}
	raw := `{"pin":"a<&b","wide":9007199254740993,"decimal":1.234567890123456789}`
	r := httptest.NewRequest("POST", v8TestBase+"/report?tag=a&tag=b", strings.NewReader(raw))
	r.Header = http.Header{"Authorization": {"Bearer management-secret"}, "Cookie": {"session=private"}, "X-Management-Key": {"management-secret"},
		"X-Api-Key": {"inference-secret"}, "X-Pacer-Cache-Capability": {"private-capability"}, "Content-Type": {"application/json"}, "Accept": {"application/json"}, "If-Match": {`"rev-8"`}}
	w := httptest.NewRecorder()
	if !h.ServeManagementV8HTTP(w, r) || w.Code != 200 || !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
	if string(p.request.Body) != raw || p.request.Path != v8TestBase+"/report" || !reflect.DeepEqual(p.request.Query["tag"], []string{"a", "b"}) {
		t.Fatal("request bytes/path/query changed")
	}
	if !reflect.DeepEqual(p.request.Headers, http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}, "If-Match": {`"rev-8"`}}) {
		t.Fatalf("unexpected input headers: %v", p.request.Headers)
	}
	wantHeaders := http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Content-Disposition": {`attachment; filename="report.json"`}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}
	if !reflect.DeepEqual(w.Header(), wantHeaders) {
		t.Fatalf("response headers=%v", w.Header())
	}
	for _, path := range []string{"/v0/management/plugins/owned/report", "/v0/management" + v8TestBase + "/report", "/v0/resource/plugins/owned/report", "/v8/resource/plugins/owned/report"} {
		if h.ServeManagementV8HTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, nil)) || h.ServeResourceHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil)) {
			t.Fatalf("alias %s dispatched", path)
		}
	}
	if p.calls != 1 {
		t.Fatalf("calls=%d", p.calls)
	}
}

func TestManagementV8RegistrationScopeLiteralMethodsAndLegacyIsolation(t *testing.T) {
	var calls atomic.Int32
	handler := managementHandlerFunc(func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
		calls.Add(1)
		return pluginapi.ManagementResponse{Body: []byte("first")}, nil
	})
	for _, tc := range []struct {
		name, id, method, path, menu string
		schema                       uint32
		accept                       bool
	}{
		{"get", "owned", "GET", v8TestBase + "/status", "", 7, true},
		{"post", "owned", "POST", v8TestBase + "/report", "", 7, true},
		{"put", "owned", "PUT", v8TestBase + "/settings", "", 7, true},
		{"patch", "owned", "PATCH", v8TestBase + "/settings", "", 7, true},
		{"delete", "owned", "DELETE", v8TestBase + "/settings", "", 7, true},
		{"wrong owner", "another", "GET", v8TestBase + "/status", "", 7, false},
		{"blank owner", "", "GET", v8TestBase + "/status", "", 7, false},
		{"dot owner", "owned..suffix", "GET", "/v8/management/plugins/owned..suffix/status", "", 7, false},
		{"owner suffix", "owned", "GET", v8TestBase + "-suffix/status", "", 7, false},
		{"menu", "owned", "GET", v8TestBase + "/status", "Public", 7, false},
		{"blank menu", "owned", "GET", v8TestBase + "/status", " ", 7, false},
		{"legacy schema", "owned", "GET", v8TestBase + "/status", "", 6, false},
		{"root", "owned", "GET", v8TestBase, "", 7, false},
		{"missing slash", "owned", "GET", "v8/management/plugins/owned/status", "", 7, false},
		{"space", "owned", "GET", v8TestBase + "/status ", "", 7, false},
		{"bad prefix", "owned", "GET", "/v8/wrong/status", "", 7, false},
		{"v8 suffix", "owned", "GET", "/v8?evil/status", "", 7, false},
		{"colon", "owned", "GET", v8TestBase + "/:id", "", 7, false},
		{"wildcard", "owned", "GET", v8TestBase + "/*path", "", 7, false},
		{"dot", "owned", "GET", v8TestBase + "/status.json", "", 7, false},
		{"traversal", "owned", "GET", v8TestBase + "/../status", "", 7, false},
		{"double slash", "owned", "GET", v8TestBase + "//status", "", 7, false},
		{"trailing slash", "owned", "GET", v8TestBase + "/status/", "", 7, false},
		{"query", "owned", "GET", v8TestBase + "/status?x=1", "", 7, false},
		{"fragment", "owned", "GET", v8TestBase + "/status#x", "", 7, false},
		{"percent", "owned", "GET", v8TestBase + "/%73tatus", "", 7, false},
		{"backslash", "owned", "GET", v8TestBase + "/a\\b", "", 7, false},
		{"long path", "owned", "GET", v8TestBase + "/" + strings.Repeat("a", 2048), "", 7, false},
		{"empty method", "owned", "", v8TestBase + "/status", "", 7, false},
		{"lower method", "owned", "get", v8TestBase + "/status", "", 7, false},
		{"head", "owned", "HEAD", v8TestBase + "/status", "", 7, false},
		{"trace", "owned", "TRACE", v8TestBase + "/status", "", 7, false},
		{"options", "owned", "OPTIONS", v8TestBase + "/status", "", 7, false},
		{"connect", "owned", "CONNECT", v8TestBase + "/status", "", 7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &managementPluginDouble{routes: []pluginapi.ManagementRoute{{Method: tc.method, Path: tc.path, Menu: tc.menu, Handler: handler}}}
			h := newHostWithRecords(capabilityRecord{id: tc.id, plugin: pluginapi.Plugin{SchemaVersion: tc.schema, Capabilities: pluginapi.Capabilities{ManagementAPI: p}}})
			h.RegisterManagementRoutes(context.Background(), nil)
			want := 0
			if tc.accept {
				want = 1
			}
			if len(h.managementRoutes) != want || len(h.resourceRoutes) != 0 {
				t.Fatalf("registered management=%v resources=%v", h.managementRoutes, h.resourceRoutes)
			}
		})
	}
	// Duplicate declarations preserve first-wins; relative and v0 declarations
	// still register only at their original v0 paths with original resource menus.
	p := &managementPluginDouble{routes: []pluginapi.ManagementRoute{
		{Method: "GET", Path: v8TestBase + "/status", Handler: handler},
		{Method: "GET", Path: v8TestBase + "/status", Handler: managementHandlerFunc(func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
			return pluginapi.ManagementResponse{Body: []byte("second")}, nil
		})},
		{Method: "GET", Path: "plugins/owned/relative", Handler: handler},
		{Method: "GET", Path: "/v0/management/plugins/owned/explicit", Handler: handler},
		{Method: "GET", Path: "/plugins/owned/menu", Menu: "Menu", Handler: handler},
	}, resources: []pluginapi.ResourceRoute{{Path: "/shell", Handler: handler}}}
	h := newHostWithRecords(capabilityRecord{id: "owned", plugin: pluginapi.Plugin{SchemaVersion: 7, Capabilities: pluginapi.Capabilities{ManagementAPI: p}}})
	h.RegisterManagementRoutes(context.Background(), nil)
	if len(h.managementRoutes) != 3 || len(h.resourceRoutes) != 2 {
		t.Fatal("legacy route table changed")
	}
	w := httptest.NewRecorder()
	h.ServeManagementV8HTTP(w, httptest.NewRequest("GET", v8TestBase+"/status", nil))
	if w.Body.String() != "first" {
		t.Fatal("duplicate priority changed")
	}
	for _, name := range []string{"relative", "explicit"} {
		if !h.ServeManagementHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v0/management/plugins/owned/"+name, nil)) || h.ServeManagementV8HTTP(httptest.NewRecorder(), httptest.NewRequest("GET", v8TestBase+"/"+name, nil)) {
			t.Fatal("legacy alias/route changed")
		}
	}
	for _, name := range []string{"menu", "shell"} {
		if !h.ServeResourceHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v0/resource/plugins/owned/"+name, nil)) {
			t.Fatal("legacy resource lost")
		}
	}
}

func TestManagementV8ReservedPatterns(t *testing.T) {
	for _, pattern := range []string{"GET /v8/management/plugins/:id/quota", "POST /v8/management/plugins/owned/*rest", "GET " + v8TestBase + "/quota"} {
		p := &managementPluginDouble{routes: []pluginapi.ManagementRoute{{Method: "PATCH", Path: v8TestBase + "/quota", Handler: managementHandlerFunc(func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
			t.Fatal("reserved handler called")
			return pluginapi.ManagementResponse{}, nil
		})}}}
		h := newHostWithRecords(capabilityRecord{id: "owned", plugin: pluginapi.Plugin{SchemaVersion: 7, Capabilities: pluginapi.Capabilities{ManagementAPI: p}}})
		h.RegisterManagementRoutes(context.Background(), map[string]struct{}{pattern: {}})
		if len(h.managementRoutes) != 0 {
			t.Fatalf("reserved pattern %s not honored", pattern)
		}
	}
	if managementV8Reserved(v8TestBase+"/quota-proof", map[string]struct{}{"GET /v8/management/plugins/:id/quota": {}}) {
		t.Fatal("literal suffix over-reserved")
	}
}

type managementV8BadReader struct{}

func (managementV8BadReader) Read([]byte) (int, error) {
	return 0, errors.New("private body reader error")
}
func (managementV8BadReader) Close() error { return nil }

func TestManagementV8RequestAndOutputBoundsErrorsAndStaleRoutes(t *testing.T) {
	p := &managementV8RPCFixture{schema: 7, routes: []pluginapi.ManagementRoute{{Method: "POST", Path: v8TestBase + "/report"}}, response: pluginapi.ManagementResponse{Body: []byte(`{"ok":true}`)}}
	h := v8RPCHost(t, p)
	for _, tc := range []struct {
		size    int
		chunked bool
		status  int
	}{{65536, false, 200}, {65537, false, 413}, {65536, true, 200}, {65537, true, 413}} {
		r := httptest.NewRequest("POST", v8TestBase+"/report", strings.NewReader(strings.Repeat("a", tc.size)))
		if tc.chunked {
			r.ContentLength = -1
		}
		before := p.calls
		w := httptest.NewRecorder()
		h.ServeManagementV8HTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("size=%d chunked=%v status=%d", tc.size, tc.chunked, w.Code)
		}
		if tc.status != 200 && p.calls != before {
			t.Fatal("oversize reached plugin")
		}
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Body = managementV8BadReader{} },
		func(r *http.Request) { r.URL.RawQuery = "a=%xx" },
		func(r *http.Request) { r.Header.Set("Accept", "text/plain\r\nInjected: private") },
	} {
		r := httptest.NewRequest("POST", v8TestBase+"/report", nil)
		mutate(r)
		before := p.calls
		w := httptest.NewRecorder()
		h.ServeManagementV8HTTP(w, r)
		if w.Code != 400 || p.calls != before || strings.Contains(w.Body.String(), "private") {
			t.Fatalf("unsafe error=%d %s", w.Code, w.Body.String())
		}
	}
	for _, path := range []string{v8TestBase + "/%72eport", v8TestBase + "/a/../report", v8TestBase + "//report", v8TestBase + "/report/"} {
		if h.ServeManagementV8HTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, nil)) {
			t.Fatalf("ambiguous path %s", path)
		}
	}
	for _, tc := range []struct {
		response pluginapi.ManagementResponse
		err      error
		status   int
	}{
		{pluginapi.ManagementResponse{Body: bytes.Repeat([]byte("x"), 8<<20)}, nil, 200},
		{pluginapi.ManagementResponse{Body: bytes.Repeat([]byte("x"), (8<<20)+1)}, nil, 502},
		{pluginapi.ManagementResponse{}, errors.New("private plugin failure"), 502},
		{pluginapi.ManagementResponse{StatusCode: 302}, nil, 502},
		{pluginapi.ManagementResponse{StatusCode: 101}, nil, 502},
		{pluginapi.ManagementResponse{StatusCode: 204, Body: []byte("private")}, nil, 502},
		{pluginapi.ManagementResponse{StatusCode: 600}, nil, 502},
		{pluginapi.ManagementResponse{Headers: http.Header{"Content-Type": {"text/html"}}}, nil, 502},
		{pluginapi.ManagementResponse{Headers: http.Header{"Content-Disposition": {"inline; filename=x"}}}, nil, 502},
		{pluginapi.ManagementResponse{Headers: http.Header{"Content-Disposition": {`attachment; filename="../x"`}}}, nil, 502},
		{pluginapi.ManagementResponse{StatusCode: 409, Body: []byte(`{"error":"pin_conflict","n":9007199254740993}`)}, nil, 409},
	} {
		p.response = tc.response
		p.err = tc.err
		w := httptest.NewRecorder()
		h.ServeManagementV8HTTP(w, httptest.NewRequest("POST", v8TestBase+"/report", nil))
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "private") {
			t.Fatalf("response=%d length=%d", w.Code, w.Body.Len())
		}
		if tc.status != 502 && !bytes.Equal(w.Body.Bytes(), tc.response.Body) {
			t.Fatal("response body changed")
		}
		if tc.status == 502 && w.Body.Len() > 100 {
			t.Fatal("oversize not replaced by sanitized error")
		}
	}
	p.err = nil
	before := p.calls
	setHostSnapshotForTest(h, false)
	if h.ServeManagementV8HTTP(httptest.NewRecorder(), httptest.NewRequest("POST", v8TestBase+"/report", nil)) || p.calls != before {
		t.Fatal("disabled/stale route dispatched")
	}
}

func TestManagementV8OlderSDKRegistrationCompatibility(t *testing.T) {
	raw, err := json.Marshal(pluginapi.ManagementRegistrationRequest{BasePath: "/v0/management"})
	if err != nil || bytes.Contains(raw, []byte("PluginManagementBasePath")) {
		t.Fatalf("nonoptional field: %s %v", raw, err)
	}
	var req pluginapi.ManagementRegistrationRequest
	if err = json.Unmarshal([]byte(`{"BasePath":"/v0/management","ResourceBasePath":"/v0/resource/plugins/owned"}`), &req); err != nil || req.PluginManagementBasePath != "" {
		t.Fatal("old host decode failed")
	}
	p := &managementV8RPCFixture{schema: 6, routes: []pluginapi.ManagementRoute{{Method: "GET", Path: v8TestBase + "/status"}, {Method: "GET", Path: "/plugins/owned/status"}}, response: pluginapi.ManagementResponse{Body: []byte(`{"value":"<&>"}`), Headers: http.Header{"Content-Type": {"application/json"}}}}
	h := v8RPCHost(t, p)
	if p.registration.PluginManagementBasePath != "" {
		t.Fatal("schema6 advertised v8")
	}
	w := httptest.NewRecorder()
	if !h.ServeManagementHTTP(w, httptest.NewRequest("GET", "/v0/management/plugins/owned/status", nil)) || w.Body.String() != string(p.response.Body) {
		t.Fatal("schema6 legacy raw handling changed")
	}
	if h.ServeManagementV8HTTP(httptest.NewRecorder(), httptest.NewRequest("GET", v8TestBase+"/status", nil)) {
		t.Fatal("schema6 v8 accepted")
	}
}
