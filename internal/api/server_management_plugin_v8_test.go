package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"golang.org/x/crypto/bcrypt"
)

const v8PluginTestBase = "/v8/management/plugins/owned"

type v8ManagementFixture struct {
	registration pluginapi.ManagementRegistrationRequest
	request      pluginapi.ManagementRequest
	calls        int
	response     pluginapi.ManagementResponse
}

func (p *v8ManagementFixture) RegisterManagement(_ context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	p.registration = req
	return pluginapi.ManagementRegistrationResponse{Routes: []pluginapi.ManagementRoute{
		{Method: "GET", Path: v8PluginTestBase + "/status", Handler: p},
		{Method: "POST", Path: v8PluginTestBase + "/report", Handler: p},
		{Method: "PATCH", Path: v8PluginTestBase + "/profiles", Handler: p},
		{Method: "GET", Path: v8PluginTestBase + "/quota", Handler: p},
		{Method: "PATCH", Path: v8PluginTestBase + "/quota", Handler: p},
		{Method: "GET", Path: v8PluginTestBase + "/menu", Menu: "Must stay private", Handler: p},
		{Method: "GET", Path: "/v8/management/plugins/other/status", Handler: p},
		{Method: "GET", Path: "/v8/malformed", Handler: p},
		{Method: "GET", Path: "/plugins/owned/legacy", Handler: p},
		{Method: "GET", Path: "/v0/management/plugins/owned/explicit", Handler: p},
	}, Resources: []pluginapi.ResourceRoute{{Path: "/shell", Handler: p}}}, nil
}

func (p *v8ManagementFixture) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	p.calls++
	p.request = req
	return p.response, nil
}

func newManagementV8PluginServer(t *testing.T) (*Server, *v8ManagementFixture) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	hash, err := bcrypt.GenerateFromPassword([]byte("dummy-management"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = string(hash)
	cfg.APIKeys = []string{"dummy-inference"}
	host := pluginhost.New()
	p := &v8ManagementFixture{response: pluginapi.ManagementResponse{Body: []byte(`{"value":9007199254740993,"text":"<&>","content":"a,b\r\n"}`)}}
	host.RegisterPluginForTest("owned", pluginapi.Plugin{SchemaVersion: 7, Metadata: pluginapi.Metadata{Name: "not-the-route-owner"}, Capabilities: pluginapi.Capabilities{ManagementAPI: p}})
	h := management.NewHandler(cfg, "", nil)
	h.SetPluginHost(host)
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h, pluginHost: host}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	s.engine.NoRoute(s.pluginManagementNoRoute)
	s.refreshPluginManagementRoutes()
	return s, p
}

func v8PluginRequest(s *Server, method, path, key string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", "session=dummy-private")
	r.Header.Set("X-Pacer-Cache-Capability", "dummy-private-capability")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	return w
}

func TestManagementV8PluginGinAuthenticationAvailabilityAndNoAliases(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		mutate    func(*Server)
		status    int
	}{
		{"management key", "dummy-management", nil, 200},
		{"missing key", "", nil, 401},
		{"unknown key", "dummy-unknown", nil, 401},
		{"inference key", "dummy-inference", nil, 401},
		{"disabled", "dummy-management", func(s *Server) { s.managementRoutesEnabled.Store(false) }, 404},
		{"home", "dummy-management", func(s *Server) { s.cfg.Home.Enabled = true }, 404},
		{"unregistered", "dummy-management", func(s *Server) { s.managementRoutesRegistered.Store(false) }, 404},
		{"no host", "dummy-management", func(s *Server) { s.pluginHost = nil }, 404},
		{"no auth handler", "dummy-management", func(s *Server) { s.mgmt = nil }, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := newManagementV8PluginServer(t)
			if tc.mutate != nil {
				tc.mutate(s)
			}
			w := v8PluginRequest(s, "GET", v8PluginTestBase+"/status", tc.key, nil)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d body=%s headers=%v", w.Code, w.Body.String(), w.Header())
			}
			wantCalls := 0
			if tc.status == 200 {
				wantCalls = 1
				if !bytes.Equal(w.Body.Bytes(), p.response.Body) {
					t.Fatal("JSON writer bytes changed")
				}
				if len(p.request.Headers) != 1 || p.request.Headers.Get("Content-Type") != "application/json" {
					t.Fatalf("headers leaked: %v", p.request.Headers)
				}
			}
			if p.calls != wantCalls {
				t.Fatalf("calls=%d", p.calls)
			}
		})
	}
	s, p := newManagementV8PluginServer(t)
	if p.registration.PluginManagementBasePath != v8PluginTestBase {
		t.Fatalf("owner from metadata rather than record: %+v", p.registration)
	}
	for _, path := range []string{
		v8PluginTestBase + "/missing", v8PluginTestBase + "/menu", "/v8/management/plugins/other/status",
		"/v8/management/plugins/not-the-route-owner/status", "/v8/management/owned-auth-url",
		"/v8/management/plugins/owned-auth-url", "/v8/management/plugins/owned..suffix/status",
		v8PluginTestBase + "/%73tatus", v8PluginTestBase + "//status", v8PluginTestBase + "/a/../status",
		v8PluginTestBase + "/status/", v8PluginTestBase + "/status.json", v8PluginTestBase + "/status%3fkey=x",
		"/v0/management/plugins/owned/status", "/v0/management/v8/malformed", "/v0/management" + v8PluginTestBase + "/status",
		"/v0/resource/plugins/owned/status", "/v0/resource/plugins/owned/menu", "/v8/resource/plugins/owned/shell",
		v8PluginTestBase + "/legacy", v8PluginTestBase + "/explicit",
	} {
		w := v8PluginRequest(s, "GET", path, "dummy-management", nil)
		if w.Code != 404 || p.calls != 0 {
			t.Fatalf("alias dispatched: %s status=%d calls=%d", path, w.Code, p.calls)
		}
	}
	for _, method := range []string{"POST", "HEAD", "OPTIONS", "TRACE", "CONNECT", "CUSTOM"} {
		w := v8PluginRequest(s, method, v8PluginTestBase+"/status", "dummy-management", nil)
		if w.Code != 404 || p.calls != 0 {
			t.Fatalf("method=%s status=%d calls=%d", method, w.Code, p.calls)
		}
	}
	// Legacy routes and their public resource stay at their original addresses.
	for _, path := range []string{"/v0/management/plugins/owned/legacy", "/v0/management/plugins/owned/explicit", "/v0/resource/plugins/owned/shell"} {
		w := v8PluginRequest(s, "GET", path, "dummy-management", nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), p.response.Body) {
			t.Fatalf("legacy route changed: %s", path)
		}
	}
}

func TestManagementV8PluginGinBuiltinsAndSingleAuthentication(t *testing.T) {
	s, p := newManagementV8PluginServer(t)
	reserved := s.registeredManagementRouteKeys()
	if _, ok := reserved["GET /v8/management/plugins/:id/quota"]; !ok {
		t.Fatal("v8 parameterized route not collected")
	}
	if _, ok := reserved["GET /v0/management/plugins/:id/quota"]; !ok {
		t.Fatal("v0 reserved routes lost")
	}
	// No quota provider exists. The fixed built-in, not the dynamic handler, owns GET.
	w := v8PluginRequest(s, "GET", v8PluginTestBase+"/quota", "dummy-management", nil)
	if w.Code == 200 || p.calls != 0 {
		t.Fatalf("plugin stole built-in: status=%d calls=%d", w.Code, p.calls)
	}
	w = v8PluginRequest(s, "PATCH", v8PluginTestBase+"/quota", "dummy-management", nil)
	if w.Code != 404 || p.calls != 0 {
		t.Fatal("plugin acquired another verb on built-in path")
	}
	// A five-failure ban happens on the sixth request, just like fixed management
	// routes. This detects accidentally invoking authentication twice per request.
	for i := 1; i <= 6; i++ {
		w = v8PluginRequest(s, "GET", v8PluginTestBase+"/status", "dummy-unknown", nil)
		want := 401
		if i == 6 {
			want = 403
		}
		if w.Code != want || p.calls != 0 {
			t.Fatalf("auth invocation %d status=%d want=%d", i, w.Code, want)
		}
	}
	// X-Management-Key is the same existing credential path, but is never forwarded.
	s, p = newManagementV8PluginServer(t)
	r := httptest.NewRequest("GET", v8PluginTestBase+"/status", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Management-Key", "dummy-management")
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	if w.Code != 200 || len(p.request.Headers) != 0 {
		t.Fatal("management header access or isolation failed")
	}
	r.RemoteAddr = "192.0.2.8:1234"
	w = httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	if w.Code != 403 || p.calls != 1 {
		t.Fatal("remote availability bypassed")
	}
}

func TestManagementV8PluginGinBoundedBodyAndResponse(t *testing.T) {
	s, p := newManagementV8PluginServer(t)
	for _, size := range []int{64 << 10, (64 << 10) + 1} {
		before := p.calls
		body := bytes.Repeat([]byte("x"), size)
		w := v8PluginRequest(s, "POST", v8PluginTestBase+"/report", "dummy-management", body)
		want := 200
		if size > 64<<10 {
			want = 413
		}
		if w.Code != want {
			t.Fatalf("body size=%d status=%d", size, w.Code)
		}
		if want == 413 && p.calls != before {
			t.Fatal("oversize body dispatched")
		}
		if want == 200 && !bytes.Equal(p.request.Body, body) {
			t.Fatal("input body changed")
		}
	}
	for _, size := range []int{8 << 20, (8 << 20) + 1} {
		p.response = pluginapi.ManagementResponse{Body: []byte(fmt.Sprintf(`{"content":"%s"}`, strings.Repeat("x", size-14)))}
		if len(p.response.Body) != size {
			t.Fatal("fixture size incorrect")
		}
		w := v8PluginRequest(s, "POST", v8PluginTestBase+"/report", "dummy-management", []byte(`{}`))
		want := 200
		if size > 8<<20 {
			want = 502
		}
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("output size=%d status=%d", size, w.Code)
		}
		if want == 200 && !bytes.Equal(w.Body.Bytes(), p.response.Body) {
			t.Fatal("output bytes changed")
		}
		if want == 502 && w.Body.Len() > 100 {
			t.Fatal("partial oversized output leaked")
		}
	}
}
