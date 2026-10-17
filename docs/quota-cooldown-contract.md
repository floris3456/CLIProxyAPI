# Additive native quota-cooldown recovery — schema 7

Delta after `d314c85d`: **no cache-policy fields, schema version or C ABI changed**.
Copy the new SDK files `sdk/pluginapi/quota_cooldown.go` and
`sdk/pluginabi/quota_cooldown.go` alongside the existing SDK. These are plugin→host callbacks
through the existing callback envelope, not plugin capabilities or HTTP/management endpoints.

| RPC | Exact Go input → result |
| --- | --- |
| `host.quota.cooldowns.read` | `HostQuotaCooldownReadRequest` → `HostQuotaCooldownReadResponse` |
| `host.quota.cooldowns.clear` | `HostQuotaCooldownClearRequest` → `HostQuotaCooldownClearResponse` |

Probe support by calling read. A pre-extension host returns its existing unsupported-callback
error even if its registration schema is 7. **Never fall back to `host.routing.reset_cooldown`**:
that older administrative callback has intentionally broader semantics and is not recovery evidence.

## Read one credential

```json
{"auth_index":"opaque-host-auth-index"}
```

Response fields: `supported`, optional `reason`, `auth_index`, `provider`, `cooldowns` array.
The initial implementation supports local Claude/Codex manager state. Home returns
`supported:false,reason:"unsupported_runtime"`; other providers return `unsupported_provider`.
No inferred zero/unlimited budget is conveyed by an empty array.

Each `HostQuotaCooldown` row has:

| Field | Type / meaning |
| --- | --- |
| `scope` | `credential` or `model` |
| `model_key` | exact stored runtime key; omitted for credential scope; never resolve aliases yourself |
| `classification` | `quota`, `non_quota`, or `unclear` |
| `basis` | `codex_usage_limit` or `claude_unified_window` when proven |
| `required_windows` | minimum exhausted windows known from the original failure |
| `origin_scope` | original failure's `credential` or `model` routing scope, including propagated rows |
| `next_retry_at` | effective runtime retry deadline, RFC3339Nano; **not a provider reset** |
| `updated_at` | manager row update time, RFC3339Nano |
| `failure_observed_at` | time the original classified failure was recorded; zero means unknown |
| `http_status` | optional redacted numeric status |
| `revision` | opaque exact-version CAS token; echo unchanged |
| `clearable` | whether this specific restriction is proven quota-only and eligible for CAS |
| `refusal_reason` | bounded host reason when not clearable |

No token material, provider error bodies, credential files or observation payloads are returned.
Auth-level aggregates are not listed as separate credential restrictions. A real credential-wide
gate and model restrictions may coexist; they are distinct rows and distinct clears.

## Clear exactly one observed row

```json
{
  "auth_index":"opaque-host-auth-index",
  "scope":"model",
  "model_key":"exact-model-key-from-read",
  "revision":"opaque-token-from-read",
  "evidence":{
    "reason":"applicable_new_cycle",
    "source":"inference_headers",
    "epoch":"observer-owned-new-provider-cycle-identity",
    "observed_at":"2026-10-08T12:00:01Z",
    "all_applicable_windows_positive":true
  }
}
```

Credential scope requires an empty/omitted `model_key`. No wildcards, all-credential reset,
TTL/compaction generation, account switch or management reset operation exists in this contract.

The trusted observation service is responsible for deciding that **every applicable window** is
fresh, source-backed, in the appropriate new cycle, non-exhausted and sufficient to serve. The
listed `required_windows` are a minimum, not permission to ignore other applicable windows.
`codex:applicable` deliberately leaves Codex primary/secondary/additional-limit applicability to
the observer. Claude window names are `claude:5h`, `claude:7d`, `claude:7d-sonnet`,
`claude:7d-opus`, `claude:7d_oi`, and/or `claude:overage`.

Host checks require reason `applicable_new_cycle`, source `inference_headers|provider_event|quota_endpoint`,
a nonempty epoch up to 256 bytes, and `all_applicable_windows_positive:true`. `observed_at` is the
actual observation receive clock, not delayed usage-callback time or a provider reset deadline.
It must be strictly after `failure_observed_at` and not after the host's current clock. These
checks reject obvious stale/invalid provenance; **CPA does not validate quota sufficiency from a
timestamp, epoch string or assertion alone**. Clock disagreement causes refusal, not implicit trust.
Do not relabel an old observation with a new time after a CAS conflict.

The result echoes `auth_index`, `scope`, and optional `model_key`, plus `cleared` and `result`:

- `cleared`: exactly that quota restriction cleared; not a promise the credential is usable.
- `stale`: version mismatch; optional `current` supplies the current redacted row.
- `not_clearable`: ambiguous/non-quota/disabled restriction; optional `current` explains it.
- `invalid_evidence`: missing, wrong, old or future provenance/assertion.
- `not_found`: credential/restriction absent or replaced with a different identity.
- `unsupported`: unsupported runtime/provider/scope, or a canceled operation.

These are successful RPC result envelopes with `cleared:false` for refusals. Invalid JSON,
unknown fields, trailing input, over-16-KiB requests, unavailable managers and unknown auth indices
use existing RPC errors. The host does not log evidence or convert refusal into a broad reset.

## Classification, concurrency and limits

CPA's historic `Quota.Exceeded`/`Reason:"quota"` covers generic 429s too. Recovery therefore records
separate process-local provenance during `Manager.MarkResult`, under its mutation lock:

- Codex: HTTP 429 with structured `usage_limit_reached` at the top level, `error.type`, or
  `response.error.type`.
- Claude: HTTP 429 with explicit `rejected` status for a recognized unified window. Aggregate
  `unified-status:rejected` alone is insufficient. Simultaneous exhausted/malformed RPM/TPM
  remaining headers are treated as mixed and cannot be cleared.
- Prior active unclassified, non-quota, differently scoped or overlapping differently classified
  backoff prevents a new failure from acquiring a quota-only recovery proof. A later identical
  ambiguous/RPM 429 invalidates proof rather than inheriting it.
- Disabled credentials/models and terminal auth errors are refused. A current non-quota auth
  error also conservatively blocks model recovery when it cannot be separated from aggregates.
- Provenance is not reconstructed from restored cooldown files, overwritten snapshots or
  replacement credentials. Those restrictions remain unclear until ordinary recovery or a fresh
  independently classifiable failure; no startup broad reset is introduced.

CAS compares instance identity, credential registration epoch, runtime generation and exact
state/provenance under the **same manager lock as new 429 results**. Even an identical timestamp
and retry deadline from a newer 429 invalidates the token. Other credential activity may also
invalidate it conservatively. Restart, replacement and token replay do not authorize clear.

Only the requested row's quota flags, retry timer and attributable quota error are removed.
Sibling model states and passive observation signals remain intact. Derived aggregate availability
is recomputed on a detached copy so even expired sibling timers are not mutated. Credential-wide
clear does not erase propagated model rows: re-read and independently assess/clear each relevant
row. Every successful clear changes the generation, so re-read before the next clear.

Scheduler and registry projections use existing generation/epoch guards, preventing a delayed
clear projection from overriding a concurrent new 429. Runtime cooldown persistence uses the
existing cooldown-state store; token files, credentials, config and management endpoints are not
written. Persistence is the existing best-effort runtime mechanism, not a new durable evidence
store. No provider calls, polling, adaptive inference or production activation are performed.
