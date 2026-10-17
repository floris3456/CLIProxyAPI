# Claude auth inventory checkpoint — schema 7

`host.auth.inventory_checkpoint` returns the auth manager's membership ledger of first-party Claude
credential tuples. It is additive: schema stays **7**, C ABI **1**. Types: `sdk/pluginapi/auth_inventory.go`,
method name: `sdk/pluginabi/auth_inventory.go`. It uses the plugin→host callback envelope with the same
caller authorization as `host.auth.list`. Requests are strict JSON (`{"provider":"claude"}`, ≤ 4 KiB,
no unknown fields or trailing input). The callback reads bounded in-memory state; it makes no provider
call and never waits for auth-store I/O.

## Response

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `provider` | Echo of the request |
| `coverage` | `known`, `unknown` or `unsupported` |
| `reason` | `overflow`, `not_ready` or `unsupported_provider`; absent when known |
| `manager_incarnation_id` | Random per auth manager instance, never reused. Restart changes it |
| `sequence` | Bumped once under the auth manager lock by every binding change; compare only within one incarnation |
| `at` | Host UTC clock when the ledger was read |
| `tuples` | Never null. Empty unless `known`. Sorted by (`auth_id`, `auth_index`) |

Tuple: `auth_id`, `auth_index` (exactly `HostAuthFileEntry.ID`/`AuthIndex` and the
`host.usage.checkpoint` scope qualifiers), `account_ref`, `binding_sequence` (the `sequence` of the
tuple's last binding change, always ≤ the response `sequence`), `state` (`active` or `removed`).

Scope: every runtime auth whose provider is `claude`, including Claude API-key entries, which stay
unknown. Disabled credentials remain `active` tuples; disabling is not a binding change.

`account_ref` is 64 lowercase hex: HMAC-SHA256 of the lowercased provider account UUID under a
32-byte key drawn from `crypto/rand` once per process. The key is never logged or exported, so refs
are comparable only within one process. Absent means unknown.

## Verified account

An account UUID is verified only when Anthropic returned it to **this process**:

- login token exchange (management OAuth flow and SDK login), carried through the persisted record;
- the profile fetched after a token refresh, when it returned a UUID;
- the request-time profile fetch, when it returned a UUID.

The executor records the observation on the returned `Auth` (`ObserveProviderAccount`); the manager
consumes it under its lock and keeps the verified UUID only in memory. Nothing is added to auth files,
metadata or `host.auth.*` output. The observation also has to match the `account_uuid` actually stored
on the credential. Unverified, so `account_ref` is absent: values loaded from the store (startup or
reload), credentials or UUIDs introduced by import, hand edit or `host.auth.save`, and CPA's own
fallback UUIDs (setup token, profile 403, profile without a UUID). A refresh whose profile lookup failed
or was refused also clears verification. No profile fetch is triggered to restore verification after
a restart.

An update without an observation keeps verification only when it changes neither the credentials
(access, refresh or id token, API key) nor the stored UUID. This covers the auth file echo of a
persisted refresh.

## Binding changes

A binding change is: a tuple appears, a tuple is removed, or its `account_ref` changes, including
known↔unknown. Each bumps `sequence` once and sets that tuple's `binding_sequence`. A refresh or token
rotation that returns the same verified UUID is **not** a binding change. Mutation points:
`Manager.Register`, `Update`/`UpdateRefreshedAuth`/`UpdatePreparedAuth`, `Remove` and `Load`.

Removed tuples stay as tombstones (`state:"removed"`, keeping their last `account_ref`) for the manager
lifetime. A re-registered tuple becomes `active` again with a new binding.

## Limits and unknowns

- More than 1,024 tuples (tombstones included) → `coverage:"unknown"`, `reason:"overflow"`, empty
  tuples, for the rest of the incarnation. The ledger never stores more than 1,024 entries.
- Auth catalog state other than `ready` (see [plugin-auth-catalog.md](plugin-auth-catalog.md)), no
  bound manager, or Home → `coverage:"unknown"`, `reason:"not_ready"`.
- Any provider other than `claude` → `coverage:"unsupported"`, `reason:"unsupported_provider"`.

## What it does not prove

The checkpoint lists the credentials this CPA process holds and the account Anthropic attributed to
them here. It does **not** prove exclusivity: claude.ai, Claude Code, other CPA installations or any
other holder of the same account are invisible to it. A known ledger is not usage coverage, quota
data or carry-in proof (see [claude-usage-checkpoint.md](claude-usage-checkpoint.md)), and an
unknown `account_ref` must not be guessed from email, file name or `account_uuid` metadata.
