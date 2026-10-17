# Claude physical source proof: local dispatch causal cuts, version 1

This is an additive **source-only** extension to [the request-start checkpoint contract](claude-usage-checkpoint.md).
Schema stays **7**, C ABI **1**, RPC stays **`host.usage.checkpoint`**. The existing request-start
`coverage`, `from`, `through`, gap clocks and source version retain their meanings. They are not
physical watermarks. No new management endpoint, provider call, timeout, wait or reset API is added.

## Narrow supported claim

For one exact first-party Claude HTTP source tuple and one manager/sink incarnation, a
`physical.coverage:"known"` response certifies two **quiescent causal cuts**. Every qualifying
local dispatch strictly between their sequences has stopped its local producer, closed its tracked
transport lifetimes, published final usage and completed acknowledged delivery to the named sink.
No scoped admission/producer/body/callback was outstanding at either cut. Thus no older **local
source producer** crosses the lower cut, including a producer whose final usage went to an old sink.

This is not a rolling physical-time watermark. Cuts order **dispatch sequences**, not RequestedAt,
FinalUsageAt or arbitrary caller-selected wall-clock boundaries:

```
physical.from_cut.sequence < SourceEvidence.physical.dispatch_sequence
                          < physical.through_cut.sequence
```

All three objects must have the same enclosing source/manager identity, and the checkpoint's sink
incarnation must be the expected consumer. Equal wall-clock timestamps do not collapse the ordering.
A future dispatch at exactly a cut's `at` still has a strictly later dispatch sequence.

The first fully quiescent read establishes a lower cut and returns **unknown**, reason
`first_quiescent_cut`. A later quiescent read with a later observed clock can return known. During
any scoped admission, producer, asynchronous body lifetime or callback, new physical proof is
unknown. There is no idle-duration rule and no fast-forward over outstanding attempts. Busy sources
may never acquire a nonempty certified interval. That is an intentional availability tradeoff.

## Wire additions

`HostUsageCheckpointResponse.physical` is an optional `UsagePhysicalCheckpoint`:

| Field | Meaning |
| --- | --- |
| `version` | `1`; reject unknown versions |
| `domain` | `cpa.claude.http.local-dispatch-sequence` |
| `coverage`, `reason` | `known` or `unknown`, with fixed bounded unknown reason |
| `from_cut` | Optional `{sequence,at}` lower causal cut; present at first initialization |
| `through_cut` | Present **only when known**; later `{sequence,at}` upper causal cut |
| `outstanding` | Admissions retained through producer shutdown and all tracked body closures |
| `pending` | Scoped final publications queued or dispatching; may overlap outstanding |
| `gap_count` | Affecting request-source/sink gaps plus physical-specific gaps |
| `invalid_from_sequence` | Omitted with gaps means **entire physical incarnation invalidated** |
| `last_producer_closed_at` | Independent observation at private `Finish`, never copied from final usage |

The enclosing exact `scope` (provider/AuthID/AuthIndex/kind/source-version), manager incarnation,
sink incarnation and issuance sequence bind this extension. The upper cut sequence equals that
issuance sequence. Missing/changed native registration, caller instance, Home runtime or fused sink
cannot expose physical cuts. Request-body parameters still accept only the existing exact scope;
there is no caller-provided physical watermark, trust flag or complete-inventory option.

`UsageSourceEvidence.physical` optionally adds `UsagePhysicalDispatch`:

- `version:1`, `admission_sequence`, `dispatch_sequence`, `started_at`.
- Issued under the shared manager lock immediately before the transport call. The original
  `upstream_started_at` remains its existing observation; this extension has its own sequence/clock.
- It can be present on an unsupported/failed attempt as an **observation**, not a coverage claim.
  Only a matching known checkpoint with no affecting gaps authorizes the causal interval.
- The manager discards publisher-supplied evidence, and callback/queue copies are detached.
  Final usage often precedes producer/body closure; the record is never mutated later to invent a
  producer-close time. The independent checkpoint gate certifies aggregate closure at its cuts.

## Creation and dispatch authority

Positive physical proof is restricted to the process-lifetime default usage manager created before
any built-in executor can run. There is no public authority setter. Arbitrary `usage.NewManager`
instances return `admission_authority_unavailable` even with an empty queue, completed mocked
records, elapsed time or caller-asserted dispatch data. The singleton is not replaceable through the
SDK or a runtime reset. A fresh process has a different manager identity and cannot reuse old cuts.

Every built-in Claude Messages/Responses generation path constructs/claims its source admission
before dispatch. Auth-manager selection admits before invocation; retries, token refresh retries
and stream-bootstrap retries use the same wrappers and get new tickets. Direct invocation of the
**concrete built-in** still constructs a ticket before it can call transport. An admitted but not yet
dispatched invocation prevents a cut. An abandoned/missing reporter creates an existing sticky gap.
Pre-construction work cannot dispatch a qualifying HTTP request through these built-in entrypoints.

At the actual `usageTTFTRoundTripper` boundary:

1. Check the concrete ClaudeExecutor type, official HTTPS `/v1/messages` POST target/Host and
   absence of replayable idempotency headers.
2. Check the private host-created fallback transport and concrete Anthropic `*http.Transport`.
   Injected/custom transports cannot qualify, including a custom `*http.Transport`.
3. Compare the actual single bearer/API-key header with the frozen selected credential hash captured
   before request construction. Tokens/hashes are not added to the proof. Changed/ambiguous headers
   create a Claude-wide **physical** identity gap because another account cannot be guessed.
4. Only the internal helper can issue the unexported concrete dispatch permit. Public SDK callers
   passing a bool, marker, string or lookalike struct cannot assert qualification. The selected tuple
   must also match the admission's immutable tuple under the manager lock.
5. Register dispatch and its body lifetimes under that lock **before** calling the transport.

Unsupported direct HTTP/count helpers, gateways, Home, custom executors/reporters and observed
untracked publications retain their existing source gaps. Repeated sends/redirects and body/transport
errors add sticky physical gaps. Unrelated account scopes remain independent unless actual identity
is ambiguous or a global tracking/clock failure prevents attribution.

## Producer and transport closure

The physical admission guard is separate from the original request-start active heap. It cannot
retire until all of the following hold:

- Explicit reporter `Finish` has observed producer shutdown; successful/failure final usage alone
  does not count. Both managed and unmanaged paths use the existing producer completion guard.
- Final usage has been enqueued under the same lock.
- The actual transport returned.
- The outgoing request body has closed, if one existed. `net/http` may do this asynchronously after
  returning a response; response completion alone cannot close this lifetime.
- The response body has closed, if one existed. Wrappers notify only **after underlying Close
  returns**. Empty `http.NoBody` retains its transport semantics and has no outstanding body lifetime.

The flight continues counting toward the existing bounded flight limit while a transport lifetime
is outstanding, even if request-start active/pending heaps are empty. Callback completion remains an
independent pending guard. No sleeps, timeout-based eviction, cancellation assumptions or queue-only
inference can release these guards. Local cancellation/errors are sticky unknown even after closure.
The cloned transport request also guards `GetBody`: a hidden transport-internal rewind invalidates
physical proof before returning a replacement body, rather than treating the first body's Close as
closure of later work. Redirect sends independently invalidate the existing single-attempt source.

At each cut, admission/dispatch, `Finish`, body closure, final enqueue, dispatch/ack, sink registration
and proof issuance all serialize on the shared manager mutex. New local dispatches cannot race
behind a cut without receiving a later sequence. The getter is bounded and never waits for I/O or
producer/callback completion. It reads counters, bounded source failures and one sink cut slot.

## Invalidation and restart rules

- Existing failed/canceled/unsupported/untracked source gaps affect the physical proof as well.
- Callback failures, missing/fused/void acknowledgments and stale completions retain the existing
  sink-qualified gap rules. Physical dispatch/body/identity failures are sticky for their source or
  manager incarnation. A successful later call, quiescent period or new cut never erases a gap.
- **Do not convert RequestedAt-based `invalid_from` to a physical sequence.** This version
  conservatively revokes the entire physical incarnation on any affecting gap; the physical
  `invalid_from_sequence` is absent. Consumers must process this even on unknown replies and revoke
  previously persisted physical proofs. Top-level gap details remain in the request-start domain.
- Reload changes sink identity and requires its own new lower cut after all pre-registration
  outstanding source work closes. A final record delivered to an old sink is not replayed or counted
  as new-sink evidence. Its producer must stop before the new lower cut can exist.
- Manager identities cannot be spliced across restarts. Separately created/replacement SDK managers
  have no lifetime authority. Other processes/installations and provider work continuing remotely
  after local shutdown are outside this **local-producer** domain; this is not provider billing end
  or proof of no external account activity.
- Wall-clock rollback retains the existing global gap. Nanosecond observations are preserved; equal
  clocks remain sequence-ordered and do not by themselves produce a positive-duration interval.

Bounds remain 1,024 credential tuples, 8,192 retained flights and 64 tracked sink slots. Physical
state adds fixed counters per scope/flight and at most one lower cut per tracked sink slot, replaced
on reload. It does not accumulate per-checkpoint history or scan usage records. Existing account,
flight, sink or gap overflow fails closed. Consumers own durable cut/record/gap persistence.

## Consumer handoff — still blocked pending separate integration

This source commit **does not close Pacer's `physical_carry_in_unproven` gate**. Its current numeric
time-window consumer is not authorized to relabel these cuts as a scalar watermark or set that flag.
A separately reviewed consumer must durably join exact dispatch sequences to these cuts, handle
late full-incarnation invalidations and sink/manager restarts, and define which analysis intervals
are valid. Arbitrary subwindows may have carry-in and cannot borrow the lower cut's empty boundary.
Per-record precise physical end times are not supplied by this extension.

Complete billing-account membership is independent and remains false for current discovery.
Counter quality, prices, provider observations, prefixes, remote/external usage and other providers
remain separate gates. No measured ratio or active routing is enabled. Native/TLS combined
acceptance is parent-owned and was not run here; positive cut tests are offline state-machine
fixtures, and injected executor transports deliberately stay unknown.

SDK copy set: **new `sdk/pluginapi/usage_physical.go`**, updated
**`sdk/pluginapi/usage_checkpoint.go`**. Existing `UsageRecord.SourceEvidence` already carries the
nested addition; no further `types.go`, ABI-method, schema or C-ABI change is needed. Core/CPAMP
source-evidence cloning carries the new detached observation automatically.
