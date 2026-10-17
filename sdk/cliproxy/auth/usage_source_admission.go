package auth

import (
	"context"
	"reflect"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Concrete host type identity, not an optional interface a plugin can advertise.
// Admission precedes executor invocation; only the built-in reporter can claim
// the context ticket. Home and custom routes are quarantined before dispatch.
func (m *Manager) usageAdmission(ctx context.Context, e ProviderExecutor, a *Auth, operation string) (context.Context, func()) {
	noop := func() {}
	if a == nil {
		if e != nil && e.Identifier() == "claude" {
			usage.DefaultManager().NoteSourceGap("claude", "", "", "missing_admission_identity", time.Now())
		}
		return ctx, noop
	}
	provider := a.Provider
	if e != nil && e.Identifier() == "claude" {
		provider = "claude"
	}
	if provider != "claude" {
		return ctx, noop
	}
	t := reflect.TypeOf(e)
	known := t != nil && t.Kind() == reflect.Pointer && t.Elem().PkgPath() == "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor" && t.Elem().Name() == "ClaudeExecutor" && a.Provider == "claude" && !m.HomeEnabled()
	// Local token counting cannot produce usage. The built-in upstream count
	// branch records its own unsupported fact immediately before its network work.
	if known && operation == "count" {
		return ctx, noop
	}
	if !known {
		usage.DefaultManager().NoteSourceGap("claude", a.ID, a.EnsureIndex(), "unsupported_executor_admission", time.Now())
		return ctx, noop
	}
	ticket, _ := usage.DefaultManager().BeginClaudeSource(nil, a.ID, a.EnsureIndex(), false)
	return usage.WithSourceAdmission(ctx, ticket), ticket.ReleaseUnclaimed
}

func (m *Manager) executeUsageSource(ctx context.Context, e ProviderExecutor, a *Auth, r executor.Request, o executor.Options) (executor.Response, error) {
	ctx, done := m.usageAdmission(ctx, e, a, "execute")
	defer done()
	return e.Execute(ctx, a, r, o)
}
func (m *Manager) streamUsageSource(ctx context.Context, e ProviderExecutor, a *Auth, r executor.Request, o executor.Options) (*executor.StreamResult, error) {
	ctx, done := m.usageAdmission(ctx, e, a, "stream")
	defer done()
	return e.ExecuteStream(ctx, a, r, o)
}
func (m *Manager) countUsageSource(ctx context.Context, e ProviderExecutor, a *Auth, r executor.Request, o executor.Options) (executor.Response, error) {
	ctx, done := m.usageAdmission(ctx, e, a, "count")
	defer done()
	return e.CountTokens(ctx, a, r, o)
}
