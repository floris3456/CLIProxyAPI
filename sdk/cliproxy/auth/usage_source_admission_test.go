package auth

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type sourceAdmissionExecutor struct {
	ProviderExecutor
	invoke func()
}

func (e sourceAdmissionExecutor) Identifier() string { return "claude" }
func (e sourceAdmissionExecutor) Execute(context.Context, *Auth, executor.Request, executor.Options) (executor.Response, error) {
	e.invoke()
	return executor.Response{}, nil
}

type sourceAdmissionSink struct{}

func (sourceAdmissionSink) HandleUsage(context.Context, usage.Record) {}

func TestPacerSourceUnsupportedExecutorGapPrecedesInvocation(t *testing.T) {
	u := usage.DefaultManager()
	name := "plugin:" + t.Name()
	u.RegisterNamed(name, sourceAdmissionSink{})
	a := &Auth{ID: t.Name(), Index: "source-admission-index", Provider: "claude"}
	_, _ = u.BeginClaudeSource(nil, a.ID, a.Index, true)
	scope := pluginapi.UsageSourceScope{Provider: "claude", AuthID: a.ID, AuthIndex: a.Index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
	before := u.SourceCheckpoint(name, scope)
	invoked := false
	e := sourceAdmissionExecutor{invoke: func() {
		invoked = true
		s := u.SourceCheckpoint(name, scope)
		if s.GapCount <= before.GapCount || s.Through != nil || s.Coverage != "unknown" {
			t.Fatalf("unsupported executor ran before gap: %+v", s)
		}
	}}
	m := NewManager(nil, nil, nil)
	if _, err := m.executeUsageSource(context.Background(), e, a, executor.Request{}, executor.Options{}); err != nil {
		t.Fatal(err)
	}
	if !invoked {
		t.Fatal("executor was not called")
	}
}
