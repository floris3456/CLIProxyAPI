package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexUsageLifecycleCorrelation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexResponseModelTestStream))
	}))
	defer server.Close()
	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			capture := &codexResponseModelUsageCapture{alias: t.Name(), records: make(chan coreusage.Record, 4)}
			coreusage.RegisterNamedPlugin(t.Name(), capture)
			t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), codexResponseModelNoopUsagePlugin{}) })
			ctx := coreusage.WithRequestedModelAlias(context.Background(), t.Name())
			auth := &cliproxyauth.Auth{ID: "test-auth", Index: "test-index", Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
			e := NewCodexExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":"hello"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), LifecycleRequestID: "logical-request"}
			if stream {
				result, err := e.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else if _, err := e.Execute(ctx, auth, req, opts); err != nil {
				t.Fatal(err)
			}
			record := capture.await(t)
			if record.LifecycleRequestID != opts.LifecycleRequestID || record.RequestID == "" || record.RequestID == record.LifecycleRequestID {
				t.Fatalf("lost logical/physical correlation: logical=%q physical=%q", record.LifecycleRequestID, record.RequestID)
			}
		})
	}
}
