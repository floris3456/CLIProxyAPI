# Native usage delivery diagnostics — additive schema 7

The unscoped API below remains unsupported. A separate subsequent
[account-qualified Claude HTTP checkpoint](claude-usage-checkpoint.md) can provide a known
source-event window under its narrower source/account/acknowledgement contract.

## Result: checkpoint coverage remains unsupported

This extension exposes real reporter/queue/callback diagnostics, **not a proven drained
provider delivery horizon**. Every response has `supported:false`, `coverage:"unsupported"`,
and **omits `through`**. Pacer must keep measured-capacity coverage unknown and **must not call
`measurement.Checkpoint` from this API's current output**. No timer, quiet period, last callback,
last acknowledged sequence, minimum pending time or empty queue may be substituted for proof.

Complete provider-wide start instrumentation is unavailable in this bounded follow-up:

- Codex duplex automatic successors can already be executing when a new reporter is created.
- Custom executors, Home/remote execution, direct SDK publication, helper/count/image paths,
  transparent transport retries/redirects and additional-model publication do not all have
  paired, host-observed physical start/end events. Provider/ExecutorType strings are not proof.
- A stream may publish its terminal usage before physical producer/body shutdown. Conversely,
  a stopped producer can still be awaiting publication. Registration can occur during either.
- A plugin may be absent/fused, lack acknowledged delivery, fail persistence, or reload while
  queued records or an old callback remain outstanding.

The implementation does not claim even Claude-wide completeness from observing its main HTTP
executors. A future positive checkpoint needs a separately reviewed complete source set and
source-event-time proof, plus durable consumer start/reload/gap markers. This API introduces no
override to promote diagnostics to known coverage.

## Exact callback and SDK delta

Plugin→host RPC: **`host.usage.delivery`**. Uses the existing native callback envelope/identity.
No plugin ID or host-callback token is supplied in the body; a plugin can read only its own named
sink. No public/management HTTP endpoint, mutation, flush, drain, wait or reset operation exists.

New files to copy alongside the existing SDK:

- `sdk/pluginapi/usage_delivery.go`: request/response/diagnostic types and optional acknowledged sink interface.
- `sdk/pluginabi/usage_delivery.go`: `MethodHostUsageDelivery`.

Schema remains **7**, C ABI remains **1**, and cache-policy/quota-recovery/usage wire fields are
unchanged. Earlier schema-7 hosts return unsupported-callback; that means unknown coverage.

Request: `HostUsageDeliveryRequest`:

```json
{"provider":"claude"}
```

Provider must be exactly `claude` or `codex`. Empty/unknown providers, unknown fields, trailing
JSON, requests over 1 KiB, missing native identity and closed native callback instances are refused.
Use a **separate background worker**, never a wait-for-self operation inside `usage.handle`.
The callback only reads cached in-memory state under short locks. It does not wait for the queue,
an inference, a network request, another plugin or a persistence operation; it never polls providers.

Response: `HostUsageDeliveryResponse`:

| Field | Meaning |
| --- | --- |
| `supported` | Always false: positive checkpoints are unsupported in this implementation |
| `coverage` | Always `unsupported`; never infer `known` from diagnostics |
| `reason` | `incomplete_source_lifecycle`, `sink_not_registered`, `sink_tracking_unavailable`, `sink_missing`, `sink_fused`; SDK also has `unsupported_provider` / `manager_unavailable` |
| `provider` | Requested provider bucket |
| `through` | Optional RFC3339Nano source-event timestamp, **always absent here** |
| `manager_incarnation_id` | Random process-local usage manager identity |
| `incarnation_id` | Random named-sink registration identity, absent before registration |
| `sequence` | Manager mutation sequence at this atomic snapshot; reads/time do not advance it |
| `registered_at` | Host registration clock, omitted before registration; never a historical coverage claim |
| `registration_sequence` | Sequence that installed the current named sink |
| `closed` | Manager stopped; subsequent publication is rejected/counted |
| `sources` | Provider-global source/queue diagnostics below, not exclusively this sink |
| `delivery` | This named sink's provider-specific delivery counters below |

Consumers receiving reordered snapshots can reject lower `sequence` within the same manager
incarnation. Across any incarnation change, previous state cannot establish continuity. Even a
higher sequence is **not** a source-time frontier. Every named registration/replacement creates
a fresh incarnation, including redundant registration; counters retain cumulative gaps/errors.

## Source and sink diagnostics

All counters are unsigned cumulative counts for this manager lifetime unless marked current.
`sources` has:

- `started`: observed Claude/Codex reporter constructions, not total upstream physical attempts.
- `open_reporters` (current): bounded retained guards awaiting final publication and/or an explicit
  producer finish. Includes unresolved paths with no instrumented finish; it is not an exact count
  of currently executing upstream requests.
- `finished_unpublished` (current): retained guards explicitly finished before successful final
  enqueue. They stay held without time-based eviction, including rejected publication.
- `earliest_reporter_start`: minimum source start among retained guards, omitted when none.
- `queued`, `dispatching` (current): all this provider's queue entries / records being dispatched.
  A dispatch stays outstanding through **all** sequential plugin callbacks, even if this sink has
  already acknowledged. Dequeue never creates a queue→dispatch accounting gap.
- `earliest_pending_start`: minimum original `RequestedAt` among tracked queue/dispatch records;
  a delayed old record remains old regardless of its enqueue or callback time.
- `published`: successfully enqueued records, including records of failed/canceled inference.
- `untracked_publications`: no matching live reporter ticket/source timestamp, including legacy
  direct publication, side-model records, repeated final records and tracking overflows.
- `rejected_publications`: publication attempts rejected by a stopped manager.
- `missing_sink_records`: records dispatched with **no plugins registered**. This cannot count
  all historical misses for a particular not-yet-registered plugin; its registration gap is explicit.
- `tracking_overflows`: retained metadata cap reached (also includes start after close / sink cap).
- `invalid_source_times`: enqueued records with zero/future source time; no synthetic replacement.
- `unknown_provider_publications`: global successfully enqueued records outside the two recognized
  provider buckets; not proof that custom sources using a recognized string are instrumented.

Minimum times only describe retained valid entries. With untracked sources, invalid clocks or
overflow they may omit an older event. They are **never a `through` substitute**.

`delivery` has cumulative `callback_started`, `callback_acknowledged`, `callback_errors`,
`callback_unacknowledged`, `callback_missing`, `callback_fused`, `stale_completions`,
`pre_registration_records`, `registration_gaps`; current `callback_in_flight`; and
`last_acknowledged_sequence` for the current registration (zero on every new registration).

- Every registration adds a gap, including the first: history before it is unproven, not empty.
- A record whose enqueue sequence or source time predates registration increments
  `pre_registration_records`, even if it subsequently persists successfully.
- Returning normally from a legacy **void** callback increments unacknowledged, never acknowledged.
- Missing/replaced/fused native adapters return explicit failures. Panics cannot count as success.
- Native `usage.handle` RPC error envelopes, transport errors and persistence failures propagate to
  the manager. An optional Go `AcknowledgedUsagePlugin.HandleUsageAcknowledged` returns error; the
  original void interface remains compatible. A nil acknowledgement must mean the plugin's write
  has completed, not that it merely queued work. CPA cannot verify a plugin's durability claim.
- An old callback finishing after re-registration increments stale completions; it cannot acknowledge
  the new incarnation. Its failure counters still accumulate. Later successful delivery never erases
  misses/errors/gaps, and no automatic replay or durable gap recovery is implied.
- `last_acknowledged_sequence` identifies an acknowledged enqueue, **not a contiguous delivery or
  source-time frontier**. It cannot be passed to checkpoint as a timestamp.

## Instrumented boundary and memory limits

`NewUsageReporter` records Claude/Codex start under the usage manager lock and uses that exact
clock as `RequestedAt`. Final Publish, producer Finish, enqueue, dequeue, named registration and
callback completion are serialized under that same lock. Final publication cannot remove a guard
before the corresponding queue item exists. Cancellation does not suppress usage publication.

Explicit physical-finish hooks cover built-in Claude HTTP Execute/ExecuteStream (including unmanaged
requests) and Codex HTTP Execute/compact/ordinary ExecuteStream. Reporter failure tracking also
settles failed-return paths. Successful uninstrumented image/WebSocket/custom paths can leave
guards unresolved; the API deliberately refuses coverage rather than guessing their completion.
Existing loopback pipeline tests remain regression coverage, not certification of missing paths.

Additional diagnostic metadata is bounded: 8,192 retained reporter guards per recognized provider,
8,192 pending source-time entries per provider bucket (including the unknown bucket), and 64 named
sink diagnostic slots. Overflow is cumulative and visible. No unbounded history/tombstones, task
goroutines, background polling, timers, network waits or durable files are added. The pre-existing
usage record queue retains its existing behavior; these limits do not drop inference/usage records.

The consumer still owns durable deduplication, persistence failures, restart/reload markers and
measured-capacity eligibility. **This follow-up must leave measurement coverage unknown.**
