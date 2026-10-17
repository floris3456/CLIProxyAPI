//go:build cgo && (linux || darwin || freebsd)

package cliproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This local ABI fixture calls the real host callback synchronously from actual
// plugin registration. No server, HTTP transport or live plugin is involved.
const startupFixtureC = `
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
typedef struct { void *ptr; size_t len; } buffer;
typedef struct { uint32_t version; void *ctx; int (*call)(void*,const char*,const uint8_t*,size_t,buffer*); void (*free_buffer)(void*,size_t); } host_api;
typedef struct { uint32_t version; int (*call)(const char*,const uint8_t*,size_t,buffer*); void (*free_buffer)(void*,size_t); void (*shutdown)(void); } plugin_api;
static host_api host;
static void release(void *ptr,size_t len) { (void)len; free(ptr); }
static int call(const char *method,const uint8_t *request,size_t len,buffer *out) {
 (void)request; (void)len;
 const char *response="{\"ok\":true,\"result\":{}}";
 if (!strcmp(method,"plugin.register") || !strcmp(method,"plugin.reconfigure")) {
  buffer observed={0};
  if (host.call(host.ctx,"host.auth.list",(const uint8_t*)"{}",2,&observed)) return 1;
  const char *path=getenv("CPA_STARTUP_FIXTURE_OUTPUT");
  FILE *file=path?fopen(path,"wb"):NULL;
  if (!file) { host.free_buffer(observed.ptr,observed.len); return 1; }
  fwrite(observed.ptr,1,observed.len,file); fclose(file); host.free_buffer(observed.ptr,observed.len);
  response="{\"ok\":true,\"result\":{\"schema_version\":7,\"metadata\":{\"name\":\"startup-fixture\",\"version\":\"1\",\"author\":\"test\",\"GitHubRepository\":\"test/local\"},\"capabilities\":{\"usage_plugin\":true}}}";
 }
 out->len=strlen(response); out->ptr=malloc(out->len); memcpy(out->ptr,response,out->len); return 0;
}
int cliproxy_plugin_init(const host_api *h,plugin_api *p) { host=*h; p->version=1;p->call=call;p->free_buffer=release;p->shutdown=NULL;return 0; }
`

type startupObservation struct {
	Files   []pluginapi.HostAuthFileEntry `json:"files"`
	Catalog *struct {
		Version       int `json:"version"`
		Domain, State string
		Generation    uint64 `json:"generation"`
		Listener      string `json:"listener_state"`
	} `json:"catalog"`
}

func startupNativeFixture(t *testing.T) (*config.Config, *pluginhost.Host, string) {
	t.Helper()
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	auths := filepath.Join(root, "auths")
	for _, dir := range []string{plugins, auths} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(auths, name+".json"), []byte(`{"type":"claude","access_token":"dummy","account_uuid":"dummy-account"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(root, "fixture.c")
	if err := os.WriteFile(source, []byte(startupFixtureC), 0600); err != nil {
		t.Fatal(err)
	}
	ext, flag := ".so", "-shared"
	if runtime.GOOS == "darwin" {
		ext, flag = ".dylib", "-dynamiclib"
	}
	command := exec.Command("cc", flag, "-fPIC", source, "-o", filepath.Join(plugins, "startup"+ext))
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile local ABI fixture: %v %s", err, out)
	}
	output := filepath.Join(root, "observed.json")
	t.Setenv("CPA_STARTUP_FIXTURE_OUTPUT", output)
	cfg := &config.Config{AuthDir: auths}
	cfg.Plugins.Enabled = true
	cfg.Plugins.Dir = plugins
	enabled := true
	cfg.Plugins.Configs = map[string]internalconfig.PluginInstanceConfig{"startup": {Enabled: &enabled}}
	host := pluginhost.New()
	t.Cleanup(host.ShutdownAll)
	return cfg, host, output
}

func readStartupObservation(t *testing.T, path string) startupObservation {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result startupObservation `json:"result"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Result
}

func TestPacerStartupActualBootstrapCatalog(t *testing.T) {
	cfg, host, output := startupNativeFixture(t)
	// Exact pre-flag path from cmd/server, before a token store/manager exists.
	host.ApplyConfig(context.Background(), cfg)
	got := readStartupObservation(t, output)
	if len(got.Files) != 2 || got.Files[0].AuthIndex != "" || got.Files[1].AuthIndex != "" {
		t.Fatalf("legacy disk fixture changed: %+v", got.Files)
	}
	if got.Catalog == nil || got.Catalog.Version != 1 || got.Catalog.Domain != "cpa.auth.disk" || got.Catalog.State != "uninitialized" || got.Catalog.Listener != "registering" {
		t.Fatalf("bootstrap registration returned %d unindexed disk entries without explicit unready provenance: %+v", len(got.Files), got.Catalog)
	}
}

type startupHeldStore struct {
	coreauth.Store
	entered, release chan struct{}
	failure          error
}

func (s *startupHeldStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.failure != nil {
		return nil, s.failure
	}
	return s.Store.List(ctx)
}

func TestPacerStartupActualServiceLoadsBeforeCatalogEnrollment(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "loaded", true: "failed"}[failed], func(t *testing.T) {
			cfg, host, output := startupNativeFixture(t)
			store := sdkauth.NewFileTokenStore()
			store.SetBaseDir(cfg.AuthDir)
			held := &startupHeldStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
			if failed {
				held.failure = errors.New("dummy store failure")
			}
			manager := coreauth.NewManager(held, nil, nil)
			service, err := NewBuilder().WithConfig(cfg).WithConfigPath(filepath.Join(t.TempDir(), "config.yaml")).WithCoreAuthManager(manager).WithPluginHost(host).Build()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sdkauth.RegisterPluginAuthParser(nil) })
			got := readStartupObservation(t, output)
			if got.Catalog == nil || got.Catalog.Domain != "cpa.auth.runtime" || got.Catalog.State != "uninitialized" || got.Catalog.Generation != 0 || got.Catalog.Listener != "registering" {
				t.Fatalf("Builder registration did not bind the actual unloaded manager: %+v", got.Catalog)
			}
			done := make(chan struct{})
			go func() { service.loadRuntimeAuth(context.Background()); close(done) }()
			select {
			case <-held.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("store load not entered")
			}
			// A plugin callback while real store I/O is held must return promptly,
			// not contend on the auth manager lock or manufacture runtime identities.
			callback := make(chan struct{})
			go func() { host.ApplyConfig(context.Background(), cfg); close(callback) }()
			select {
			case <-callback:
			case <-time.After(5 * time.Second):
				close(held.release)
				t.Fatal("catalog callback waited on auth loading")
			}
			got = readStartupObservation(t, output)
			if got.Catalog.State != "loading" || got.Catalog.Generation != 1 || got.Catalog.Listener != "installing" {
				t.Fatalf("load/registration state: %+v", got.Catalog)
			}
			close(held.release)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("auth load did not finish")
			}
			host.RegisterUsagePlugins()
			t.Cleanup(func() { host.ShutdownAll() })
			host.ApplyConfig(context.Background(), cfg)
			got = readStartupObservation(t, output)
			if got.Catalog.Listener != "ready" {
				t.Fatalf("actual listener not installed: %+v", got.Catalog)
			}
			if failed {
				if got.Catalog.State != "failed" || len(got.Files) != 0 {
					t.Fatalf("failed startup claimed ready/history: %+v", got)
				}
				return
			}
			if got.Catalog.State != "ready" || len(got.Files) != 2 {
				t.Fatalf("loaded catalog: %+v", got)
			}
			for _, file := range got.Files {
				a, ok := manager.GetByID(file.ID)
				if !ok || file.AuthIndex == "" || a.Index != file.AuthIndex {
					t.Fatalf("catalog did not match registered immutable index: %+v", file)
				}
			}
			before := got.Files
			// A real file-store reload, not a manually-ready toggle, retains indices.
			held.entered = make(chan struct{})
			held.release = make(chan struct{})
			close(held.release)
			service.loadRuntimeAuth(context.Background())
			host.ApplyConfig(context.Background(), cfg)
			got = readStartupObservation(t, output)
			if got.Catalog.State != "ready" || got.Catalog.Generation != 2 {
				t.Fatalf("reload catalog: %+v", got.Catalog)
			}
			for i, file := range got.Files {
				if file.ID != before[i].ID || file.AuthIndex != before[i].AuthIndex {
					t.Fatal("reload changed source identity")
				}
			}
		})
	}
}
