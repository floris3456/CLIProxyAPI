# Claude assigned response-tier observation

The Messages request `service_tier` (`auto` or `standard_only`) chooses permitted
capacity; it does not establish the tier assigned to that response. CPA reads the
assigned observation only from response `usage.service_tier`, including nested
`message_start.message.usage` and subsequent `message_delta.usage` events.

## Parsing and stream resolution

Accepted strings are exactly `standard`, `priority`, and `batch`, matching the
current official Usage enum. No trimming, case repair, model-based inference,
request-tier substitution or default-price inference is performed.

- No observed tier: empty `ResponseServiceTier` (unknown).
- A later absent field is no new observation; prior valid tier survives usage-only
  updates. A first valid late observation can establish the assigned tier.
- Explicit null, non-string, unsupported value or duplicate tier key is invalid.
  Conflicting valid tiers are invalid too. The Claude stream buffer latches unknown
  for the response and later valid observations cannot heal it.
- Malformed JSON is not accepted as tier evidence. A malformed JSON object in the
  Claude stream invalidates its tier observation without discarding retained usage.
- Tier-only objects, including empty optional count containers, do not produce
  complete zero accounting. They preserve existing counters, reasoning, write split
  and quality; a metadata-only first observation is unclassified, including at publish.
- Actual usage updates retain existing Claude independent-category accounting and
  cache-create snapshot merging. Generic OpenAI/Codex tier-resolution logic is unchanged.

The state is bounded to one enum string and one sticky invalidity bit per stream.
No SDK fields, ABI/schema change, timeout, pricing rule, persistence migration or
new general-purpose format module is required.

## Existing publication path

`parseClaudeUsageNode` → `usage.Detail.ResponseServiceTier` → Claude stream buffer
when applicable → `UsageReporter` / `usage.Record.ResponseServiceTier` → native
usage adapter → SDK `pluginapi.UsageRecord.ResponseServiceTier` / raw usage RPC.
The existing `ServiceTier` field continues to represent the request separately.

Pacer's existing `FromNative` retains the assigned value in
`Native.ResponseServiceTier` and `Attribution.ServiceTier`, with requested value in
`Native.RequestedServiceTier`. Its pinned forward converter uses
`Attribution.ServiceTier` as the existing event pricing-tier input. This patch adds
no `ChargedServiceTier` field and does not change forward valuation/schema. For the
native `auto` case, absence remains `charged_tier_unknown`, not a priced default.

An observed assigned tier can select an independently qualified price rule. It does
not prove provider invoicing, account completeness, physical time coverage or warmth.
Existing unknown immutable records are not backfilled; a replay changing captured
tier/attribution remains an immutable-evidence conflict.

## Sources checked 2026-10-08

- [Official service tiers](https://platform.claude.com/docs/en/api/service-tiers),
  searched with Tavily and extracted in full: request semantics and response usage
  assignment. The model-support list does not replace an actual response observation.
- [Official Python Usage type](https://github.com/anthropics/anthropic-sdk-python/blob/main/src/anthropic/types/usage.py),
  searched with Exa and extracted in full: `Optional[Literal["standard", "priority", "batch"]]`.

## Source-only evidence

`helps/claude_response_tier_test.go` (under `internal/runtime/executor`) reproduces
pre-fix loss through the actual publisher and tests strict parsing, sticky conflicts,
late optional observation, tier-only accounting and exact counts/reasoning/splits.
`internal/pluginhost/claude_response_tier_test.go` runs the real Claude executor with
a fake RoundTripper through the real host usage adapter and RPC marshaler, inspecting
the raw SDK callback for JSON/SSE positive and unknown/conflicting cases.

These tests make no provider or active-lab requests. Matching native acceptance is
parent-owned and requires a preservation plan for the original in-memory fake fixture.
