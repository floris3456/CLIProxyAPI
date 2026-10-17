# Account-qualified Claude HTTP usage checkpoints — schema 7

## Exact supported claim

`host.usage.checkpoint` can return **`coverage:"known"`** for this one immutable source:

```json
{"provider":"claude","auth_id":"runtime-credential-id","auth_index":"opaque-index","kind":"cpa.claude.http.first-party","version":1}
```

The proof covers source **request-start event time in `[from, through)`**, for that exact runtime
credential tuple and the named native consumer incarnation. It means every included attempt started
in that interval has stopped its producer and its final usage publication has completed acknowledged
delivery. A currently active or queued attempt at exactly `through` is excluded. Equal `from` and
`through` is an empty window, not a capacity sample.

This is **not all-CPA coverage, provider-account total usage, complete provider quota observations,
or proof of token/cost accuracy**. Codex, Home, custom executors, other endpoints and older records
remain outside this supported source. Matching-account unsupported activity invalidates the scoped
proof when observed/admitted; unrelated accounts/providers are not automatically invalidated.
The original unscoped `host.usage.delivery` remains diagnostics-only and unsupported.

An additive optional `physical` extension now describes **local-dispatch causal cuts**, with
independent admission/body/producer closure authority. See [the physical source contract](claude-physical-source-proof.md).
It does not change this document's request-start `coverage`, `[from,through)` or gap-clock semantics,
and must not be substituted for a numeric physical watermark by existing consumers.

`auth_id` / `auth_index` are exactly `HostAuthFileEntry.ID` / `AuthIndex` and native usage
`AuthID` / `AuthIndex`. They are **not a newly discovered provider billing AccountID**. No raw
credential, email or token is returned. A consumer with a different durable AccountID must explicitly
map that account to these qualifiers; shared billing accounts, duplicate credentials and activity
outside this CPA source cannot be assumed covered. That remains a measurement eligibility gate.

## SDK and ABI delta

New files: `sdk/pluginapi/usage_checkpoint.go`, `sdk/pluginabi/usage_checkpoint.go`.
One additive native usage field in `sdk/pluginapi/types.go`: `UsageRecord.SourceEvidence`
(`*UsageSourceEvidence`). Its outer JSON key follows the existing native PascalCase usage envelope;
the new nested types use snake_case. The core record and CPAMP queue carry the same frozen evidence
(`source_evidence` in the queue). Existing cache-policy fields and quota callbacks are unchanged.

The optional physical extension adds `sdk/pluginapi/usage_physical.go` and nested `physical` members
to this file's evidence/response structs. Copy both pluginapi checkpoint files together; ABI/schema
versions and callback names are unchanged. Missing extension remains physically unknown.

Schema remains **7**, C ABI **1**. This builds on the `c9acb5ab` quota and `f755bb08` delivery work;
a consumer copying SDK files from a `d314c85d` baseline needs those prior additive files too.
An older host's unsupported callback or missing evidence means unknown, not an inferred horizon.

## Background callback

RPC: **`host.usage.checkpoint`**, using the existing plugin→host callback envelope. Request:

```json
{
  "scope": {
    "provider":"claude",
    "auth_id":"runtime-credential-id",
    "auth_index":"opaque-index",
    "kind":"cpa.claude.http.first-party",
    "version":1
  }
}
```

The native caller identity selects its sink; there is no body-supplied plugin ID. The host checks
both credential identifiers against the auth manager, pins the current usage adapter registration,
and checks native callback instance identity. A newly loaded plugin cannot borrow the old sink's
proof before its own usage registration. Closed caller instances, malformed/unknown JSON fields,
trailing input, over-4-KiB requests and mixed credential tuples are refused. Home is unsupported.

Read from a **separate background worker**, not a self-drain inside `usage.handle`. The callback
reads bounded in-memory state under locks; it never waits for network/usage/persistence or polls
providers. Inference never waits for the proof to drain.

Response `HostUsageCheckpointResponse`:

| Field | Meaning |
| --- | --- |
| `scope` | Exact qualifiers above; must match the requested measurement source |
| `coverage` | `known`, `unknown` or `unsupported` |
| `reason` | Bounded refusal/unknown reason; empty for known |
| `manager_incarnation_id` | Host usage-manager identity; restart changes it |
| `incarnation_id` | Named sink registration identity; every registration/reload changes it |
| `sequence` | Ordered checkpoint-issuance/host-mutation sequence; compare only within the manager incarnation |
| `from` | Source/consumer lower boundary; pre-existing sources use registration time + 1 ns to exclude equal-time history; absent if unavailable |
| `through` | **Present only for known coverage**, exclusive source-event-time bound |
| `active` | Current retained scoped producer/admission guards |
| `pending` | Current scoped publications queued or dispatching |
| `gap_count` | Source/global/current-sink gap observations affecting this scope |
| `invalid_from` | Earliest invalidated source time across all affecting gaps, even omitted ones; absence with gaps means unknown beginning |
| `gaps` | Up to 16 detached gap entries, each with `sequence`, `reason`, `from`, `through`, `observed_at` |
| `gaps_truncated` | The bounded list omits entries; never interpret this as no further gaps |

Unknown reasons include `source_not_enrolled`, `sink_not_registered`, `sink_tracking_unavailable`,
`sink_registration_changed`, `sink_instance_mismatch`, `sink_unavailable`, `pre_window_outstanding`,
`delivery_gap`, `manager_closed`. Unsupported reasons include `unsupported_source` and
`unsupported_runtime`. Both unknown and unsupported **omit `through`**. No start is inferred from
registration and no pre-enrollment history is certified.

## What establishes the proof

1. The auth manager admits built-in Claude attempts **before executor invocation**, including
   credential/model retries, stream-bootstrap retries and refreshed-credential retries. It recognizes
   the concrete built-in Go type, not a plugin-advertised capability. Unsupported selected executors
   and Home record an account-qualified gap before dispatch. Local built-in token counting is not
   generation; its upstream-count branch records a gap before network work.
2. Both built-in Claude generation entry points use a dedicated reporter constructor. It claims the
   admission once or atomically creates its own start for direct built-in invocation. The claim freezes
   account/source/start identity; request retries get new tickets and physical usage IDs. Managed,
   unmanaged, summary/compaction-purpose, Messages/Responses and stream/nonstream requests share it.
3. Only exact HTTPS `api.anthropic.com` `/v1/messages` traffic qualifies. First-party URL checks do not
   trust executor-name strings from a published record. Gateways, explicit alternate hosts/ports,
   custom injected RoundTrippers, raw HTTP helpers and upstream count helpers remain unproven.
4. The existing real RoundTrip wrapper captures `upstream_started_at` immediately before transport,
   independently of cache policy. Repeated sends/redirects or replayable idempotency headers invalidate
   the account scope instead of pretending multiple sends are one fully observed attempt.
5. Healthy producer guards remain until body/producer shutdown **and** final enqueue. Stream terminal usage
   alone cannot release a guard. Canceled/truncated/failed attempts and absent/zero-only usage create
   conservative account gaps; successful missing-usage branches still emit a marked unknown record.
   Clean unmanaged SSE EOF without `message_stop` and nonstream responses lacking protocol completion
   also emit gaps, independently of legacy response-forwarding success.
   An unclaimed admission records a missing-reporter gap before retiring its abandoned guards;
   it never invents a successful final record.
6. Admission/claim, final Publish/Finish, queue transition, callback result, source-gap insertion and
   checkpoint snapshot share the usage manager lock. A final publication is in the pending set before
   any finished guard disappears; dequeue retains it until all sequential callbacks complete.
7. Native persistence/RPC errors, missing/fused/void callbacks and stale callback completion after
   reload cannot count as success. Sink failure is scoped to that consumer/account. A slow unrelated
   callback can delay the conservative bound, but its failure cannot acknowledge Pacer's delivery.

Under that lock the host reads wall-clock `now` and computes:

```
through = min(now, oldest active RequestedAt, oldest queued/dispatching RequestedAt)
```

An empty set permits `now` **because all included future starts are registered under the same lock
with a non-earlier source clock**, not because the queue was quiet for a duration. Times are normalized
to UTC without Go monotonic components; a wall-clock rollback emits a gap and removes known coverage.
An older outstanding start before `from` yields unknown until resolved; no clock-based eviction occurs.
Registration sequencing, not wall-clock equality, distinguishes old history from new admission.
Preserve nanosecond bounds, or round `from` up and `through` down when storing coarser times;
rounding must never expand the proven window.

## Frozen usage evidence and interval gaps

`SourceEvidence` contains `scope`, `manager_incarnation_id`, `tracked`, `requested_at`, optional
`upstream_started_at`, `final_usage_at`, optional `gap`.

- The manager creates this evidence from the private live ticket and verifies provider, both account
  identifiers and original source start. Caller-supplied evidence is discarded, including forged
  `tracked:true`. Every sink receives a deep copy.
- `requested_at` is the admission/reporter clock, conservatively before upstream. It drives the
  delivery bound, not a delayed usage-callback clock. `upstream_started_at` is the newly observed
  dispatch clock, including for unmanaged requests; absence is not backfilled from policy or latency.
- `final_usage_at` is publication/usage collection time, not a provider reset or exact billing end.
  Older/no-marker paths have unknown interval provenance. Counter quality and interval applicability
  still need validation by the native converter; tracked delivery is not sufficient by itself.
- Untracked publication emits `tracked:false` and a gap from its original request time. A delayed
  old record can invalidate a **previously returned** horizon. Missing/zero/future source times give
  an unknown gap beginning. The checkpoint also exposes the invalidation, even if usage persistence
  failed or an unsupported executor never published a record.
- Unknown publishers with a complete tuple invalidate matching accounts. Partial identifiers
  invalidate all matching tuples. Missing Claude account identity creates a Claude-wide gap. Other
  providers only affect a Claude scope if their supplied account identifiers match it.
- These gaps are deliberately **open-ended and sticky for the affected manager/source incarnation**:
  no timestamp-only end, retry, idle interval, successful later callback or reset RPC repairs a missing
  source/persistence fact. Failed/canceled source usage is conservatively quarantined too. A future
  reconciled closed-gap/recovery protocol is outside this change; unrelated account scopes can stay known.

## Consumer acceptance and remaining boundaries

Before advancing a scoped measurement checkpoint, require exact scope/version, `coverage:"known"`,
both incarnation IDs, valid `from <= through`, the expected sink identity, and non-stale sequence.
Persist/read gap information **even on unknown responses**, and invalidate affected stored proof or
measurement intervals from `invalid_from` (unknown beginning means the whole incarnation). A shorter
gap list or a later successful record is not recovery. Keep durable consumer start/reload/gap markers.

Use the checkpoint only for a measurement interval eligible under these source/account qualifiers.
Pre-window/carry-in intervals, incomplete counters, actual dispatch-time applicability, account alias
mapping and complete applicable provider windows remain native measurement gates. Nothing here
certifies arbitrary direct SDK executors, plugin-owned HTTP traffic, Home remote attempts, Codex,
other installations, external usage of the same provider account, or unobserved billing identities.

Metadata limits: 1,024 source credential tuples, 1,024 unknown-account selectors, 8,192 total retained
source flights (active or pending), 16 retained gaps per failure set and at most 64 failing sink
incarnations per account. Overflow produces unknown coverage; earliest invalidation is retained even
when individual gap entries are omitted. The existing usage queue itself is not dropped or re-sized.
No durable files, provider polls, timers, production activation or native candidate rebuild are added.
