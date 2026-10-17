# Pacer CPA boundary: native schema 7, client protocol 1 / contract revision 2

Additive post-`d314c85d` delta: [quota-cooldown read/CAS-clear callbacks](quota-cooldown-contract.md).
New SDK files `sdk/pluginapi/quota_cooldown.go` and `sdk/pluginabi/quota_cooldown.go`; schema 7,
C ABI and all cache-policy structs below are unchanged.

Further additive delta: [usage delivery diagnostics](usage-delivery-contract.md). This returns
unsupported checkpoint coverage with no `through`; it must not enable measured-capacity ratios.

The subsequent [account-qualified Claude HTTP source checkpoint](claude-usage-checkpoint.md)
adds `host.usage.checkpoint` and `UsageRecord.SourceEvidence`. Only that narrower API can return a
known `[from,through)` source-event window; unscoped diagnostics and Codex remain unsupported.

Implemented CPA transport, final enforcement and usage plumbing; **no durable policy store**.
Aligned with the Pacer policy module's `fcf9dd5c904ae05ede901bd9e7f86fe86f283376`
`docs/cache-policy-contract.md`. Pacer owns `Open` on startup, explicit first-install `Create`,
scope/capability authorization, CAS/compaction, durable request/attempt receipts and recovery.

## Native registration and discovery

Plugin ID **`pacer`**, registration `schema_version:7`, `capabilities.cache_policy:true`.
Implement `pluginapi.CachePolicyProvider` (exact structs: `sdk/pluginapi/cache_policy.go`):

| RPC | Go request → response | Bridge operation |
|---|---|---|
| `cache_policy.handle` | `CachePolicyRequest` → `CachePolicyResponse` | GET `Store.Get`; POST `Store.Apply` |
| `cache_policy.resolve` | `CachePolicyResolveRequest` → `CachePolicy` | authorize reference, `Store.BeginRequest` |
| `cache_policy.attempt` | `CachePolicyAttemptRequest` → `CachePolicyAttemptResponse` | `begin`, `validate`, `finish` |
| existing `request.complete` | existing `RequestCompletion` | `Store.FinishRequest` |

Register `request_lifecycle_plugin:true` and `usage_plugin:true` too; resolution refuses a plugin
without logical completion support. Schema 7 host registration
is the minimum feature gate for selection. Existing C ABI and JSON envelope are unchanged;
these new structs use explicit **snake_case** fields. Existing usage/completion fields retain
their existing **PascalCase** JSON names. Older/fused/missing plugins fail closed for managed
requests. Routing mode does not disable validation. Successful HTTP policy responses advertise
`X-Pacer-Cache-Protocol:1`, `X-Pacer-Cache-Contract-Revision:2` and
`X-Pacer-Cache-Attempts:begin-validate-finish-v1`; Pacer's capability
view should advertise selection only with schema 7 and compatible durable state.

`scope.caller_scope` is 64 lowercase hex, using CPA's existing
`session.CallerScope(principal)` = SHA256(`"cli-proxy-api:caller-scope:v1\x00" + authenticated
principal). Pass it as `Credentials.CallerDigest`; do not hash it again. No raw inference key or
arbitrary inbound headers is passed in these RPCs. Capabilities come from a private host closure
for every attempt, never mutable metadata. They are absent from policy snapshots and usage.

### Physical-attempt lifecycle

1. `resolve` gets scope, private capability, epoch/revision, kind, lifecycle `request_id`, model,
   source format and stream flag. No client TTL exists: Pacer may `Get` the active TTL, then call
   `BeginRequest` with that TTL and the supplied epoch/CAS (which the store must validate).
   Return `CachePolicy{protocol,scope,context_epoch,revision,ttl,kind}`.
2. Before executor processing, `attempt` **begin** gets that frozen policy, `request_id`, fresh
   UUID `attempt_id` and capability. Call `BeginAttempt`. Return `policy` (identical to resolve)
   plus its durable `started_at`. Admission failure prevents dispatch, including retries during
   prepare. Each retry gets a new physical UUID; an existing attempt is never redispatched.
3. After all body/header rewrites and CCH re-signing, **validate** gets the same policy/IDs and
   exact `started_at`; reconstruct the store Snapshot (`Managed:true`) and call `ValidateAttempt`.
   Return `{}` on success. Failure prohibits upstream dispatch.
4. **finish** runs synchronously after transport stops and the upstream body closes (including
   the streaming goroutine), with `outcome:succeeded|failed|canceled`, `dispatched`, and the same
   immutable reference/IDs/start. Call `FinishAttempt`; return `{}`. Failed settlement is surfaced
   and the durable guard must remain outstanding. No timeout/restart is automatic settlement.
5. Existing `request.complete.RequestID` is the same lifecycle ID; CPA delays this notification
   during cancellation until all process-local physical attempts have stopped. Settle the logical request
   only after all attempt guards settle. Keep private credentials by lifecycle ID inside Pacer
   until completion/recovery. Usage is a separate asynchronous fact, **not a settlement callback**.

## HTTP policy API

Exact `/v1/plugins/pacer/cache-policy`; standard inference authentication, required even when
legacy anonymous inference is enabled. Management/resource auth is unchanged. Deployment must
provide authenticated HTTPS/trusted ingress as for inference; the host does not trust a caller's
Forwarded header as proof of HTTPS. Only GET read and POST JSON operations dispatch.

GET query: `protocol=1&installation_id=…&session_id=…&provider_route=…` (no extra/duplicate fields).
All calls, **including initial ensure**, require `X-Pacer-Cache-Capability`: canonical unpadded
base64url of exactly 32 client-generated random bytes. Never return it in response JSON.
Missing capability is 403; malformed capability/headers/body is 400. POST JSON max **16 KiB**;
unknown/duplicate/case-aliased/null fields, trailing JSON and query parameters are rejected.

Required POST fields: `protocol`, `operation` (`ensure|prepare|commit|cancel`), `installation_id`,
`session_id`, `provider_route`, `context_epoch`, `expected_revision` (>=0), `ttl` (`5m|1h`),
`operation_id`. IDs are 1–256 printable ASCII bytes without whitespace. Optional fields:
`enrollment:"new_empty"` (ensure only) or `"existing_history"` (prepare only);
`transition_id` (required for commit/cancel); `checkpoint` and `checkpoint_receipt` (required for
commit). Commit accepts optional `compaction_started_at` and `checkpoint_committed_at` RFC3339Nano
provenance; it does **not** require or compare client/server clocks. Commit `context_epoch` is the
**new** epoch and equals `checkpoint`. Exact `CachePolicyCheckpointReceipt` JSON (all fields
required, no unknown/duplicate/null keys):

```json
{"version":1,"transition_id":"prepared-transition","previous_checkpoint":"old-checkpoint","checkpoint":"new-checkpoint","committed":true}
```

The receipt is the client's durable Core acknowledgement, not proof of summary content. Pacer
checks it against the pending transition and old checkpoint, current CAS and settled guards.
The client binds prepare before compaction and durably retains the completed receipt before commit;
timestamps, summaries and generation changes cannot replace it. CPA transports this receipt intact.
Prepare increments CAS; pass its returned revision into commit/cancel. No `new_context_epoch`,
`empty_context`, `caller_identity`, `generation` or `skip_compaction` field exists.

`CachePolicyRequest.operation` is the decoded command and `scope` has the host caller digest.
The `enrollment` label alone is **not trusted empty-context evidence**. CPA does not currently issue
an enrollment attestation; a bridge accepting a trusted authenticated client-plugin's new-empty
attestation must establish that trust separately. Otherwise it must pass `Evidence.NewEmpty:false`
and return 422 enrollment-unsupported. Do not infer empty from missing state. Existing/unknown/copied
history can instead use `prepare` with `enrollment:existing_history`, then receipt-based commit.
The bridge keeps managed inference blocked while that enrollment guard has unknown active TTL.

Return policy `State` directly in `CachePolicyResponse.body` (`json.RawMessage`, not base64),
with explicit `status_code`. Responses are bounded to 16 KiB, JSON, `no-store`, and cannot set
arbitrary headers. Domain error JSON can be returned here with its specific code. RPC failures
are redacted to host policy-rejected/unavailable errors while retaining safe 4xx status.

## Managed inference and final boundary

Eight headers: `X-Pacer-Cache-Protocol:1`, `-Installation`, `-Session`, `-Route`, `-Epoch`,
`-Revision` (>=1), `-Kind:primary|warming`, `-Capability`. Any header in this namespace opts into
fail-closed handling. `provider_route` is a Pacer-configured identifier (e.g. `claude-main`),
not permission to change executor; Pacer validates its mapping. Host admits only built-in Claude
HTTP POST `/v1/messages` or `/v1/responses`, stream/non-stream. Mixed/other providers, direct
plugin executors, Home, WebSockets, count/compact and recognized probe/title helpers are refused.
Auxiliary calls use a separate unmanaged scope; no primary policy is borrowed for summaries.

All eligible caller/CPA markers (tools, system, message content, top-level automatic cache) get
explicit 5m/1h after translation, payload overrides, placement, normalization and helper shaping.
CCH is re-signed, then final body/headers and durable attempt are validated before dispatch.
1h gets `extended-cache-ttl-2025-04-11`. Official HTTPS `api.anthropic.com` and loopback test
upstreams only; unverified gateways and managed redirects are refused. Tool schema/input text
is not traversed as cache metadata. No private policy headers go upstream. No markers means no
effective cache policy, **not** a successful full-context refresh. Unmanaged clients keep behavior.

## Usage and quota observation clocks

`UsageRecord.RequestID` is the physical UUID; `LifecycleRequestID` joins resolve/completion.
`CachePolicy` evidence has frozen `Selected`, durable `AttemptStartedAt`, actual dispatch
`UpstreamStartedAt`, `Dispatched`, final `EffectiveTTL` and `BreakpointCount`. Selected TTL never
fills reported usage. `Detail.CacheCreation` has optional aggregate/5m/1h pointers, optional
unknown remainder and quality; missing != zero, inconsistent totals remain flagged. Streaming
cumulative snapshots merge by field without adding duplicates, including partial failures.
Truncated managed streams fail even after a usage-bearing message_delta; native nonstream body-read
errors and translated nonstream streams preserve the counters received before failure.

Native usage additionally carries accounting schema 2/version, normalized `TokenBreakdown`,
client attribution/topology and access-token fingerprint. CPAMP queue additions:
`lifecycle_request_id`, `cache_policy`, `tokens.cache_creation`, and
`response_headers_observed_at`. Existing queue `execution_id` remains the physical UUID.
Native `ResponseHeadersObservedAt` maps canonical header names to actual HTTP-response/event
receive clocks, captured before delayed usage dispatch. Later Codex events update only their
own header clocks. Anchor relative resets to each header's clock, **never callback time**;
missing clocks remain unknown. Header values and clocks are snapshotted atomically.

Delivery remains in-memory; consumers persist/deduplicate physical IDs and report gaps. No quota
polling, durable policy state, client UI, enrollment attestation, deployment or live-provider
acceptance is implemented by this CPA boundary.
