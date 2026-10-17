package pluginabi

// Additive schema-7 callbacks. Older hosts return unsupported host callback;
// callers must never fall back to host.routing.reset_cooldown.
const (
	MethodHostQuotaCooldownRead  = "host.quota.cooldowns.read"
	MethodHostQuotaCooldownClear = "host.quota.cooldowns.clear"
)
