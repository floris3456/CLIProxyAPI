# Auth catalog startup provenance (additive version 1)

`host.auth.list` retains `files` and adds `catalog`:

```json
{"catalog":{"version":1,"domain":"cpa.auth.runtime","state":"ready","generation":1,"listener_state":"ready"},"files":[]}
```

Native schema 7 and C ABI 1 are unchanged. No physical SDK types, checkpoint
request-time semantics, management routes or readiness/trust override are added.

## Why initialization cannot simply be reversed

`cmd/server` loads plugins before flag parsing to register plugin-owned flags, and
again before building the service. The token store is selected after the first
load. `Builder.Build` formerly also registered plugins before constructing/binding
the core auth manager. Thus registration-time `host.auth.list` legitimately used
the legacy disk fallback; those entries have no runtime `AuthIndex`.

The full auth-store load itself needs plugin auth parsers: `FileTokenStore.List`
calls the registered parser before falling back to built-in parsing. Deferring all
plugin registration until after that load would change credential parsing and
catalog visibility. This change binds the real manager before Builder callbacks,
keeps early CLI registration and disk fallback, and explicitly labels provenance.

`Service.loadRuntimeAuth` is the existing pre-network load boundary factored from
`Run`: bind the manager, register plugin parsers, load the actual store, register
configured credentials and restore cooldown state. No new I/O, retries or timers
are added. Load failures retain their existing log behavior and report `failed`.

## Fields and authority

- `version`: exactly `1` for this contract; unknown versions confer no readiness.
- `domain`: `cpa.auth.disk` when no runtime manager is bound; otherwise
  `cpa.auth.runtime`. Disk entries remain visible and never acquire invented indices.
- `state`: `uninitialized`, `loading`, `ready`, `failed`, `unavailable`, or
  `unsupported` (Home). Only actual `Manager.Load` completion publishes `ready`.
  `Register`/inventory count/time passage cannot do so. There is no readiness setter.
- `generation`: uint64 load-attempt generation of this auth manager, starting at 1;
  0 before loading. It is not the usage-manager incarnation, an auth index, a source
  sequence, a billing membership revision or a physical watermark.
- `listener_state`: `registering` for the actual loading callback instance;
  `installing` when its active usage capability lacks the matching registered
  usage adapter; `ready` only for that installed instance; `retired` or
  `unavailable` otherwise. It is independent of auth loading and producer quiescence.

During an auth-store load, the catalog retains its prior cloned runtime entries,
explicitly marked `loading`; it does not wait for store I/O or a plugin parser.
The snapshot gate orders reads against the start of loading; it does not replace
a busy runtime catalog with an empty result. Unavailable/error states must never
be interpreted as harmless startup.
Loading/failure snapshots do not expose raw credentials beyond existing file metadata.

Readiness certifies neither complete billing-account membership nor admission,
usage delivery, pricing, credential continuity over an interval or physical closure.
The existing exact scope/manager/sink checkpoint and immutable usage joins remain
the only physical-proof path. A pre-install checkpoint remains unknown, not an
acknowledgment of an unregistered listener. A newly installed but unobserved source
returns `source_not_enrolled` and has no physical cut.

## Consumer rule and ordered native acceptance

Pacer may defer initial enrollment only on a supported, explicitly uninitialized or
loading catalog, or while its actual usage listener is registering/installing.
After any runtime/legacy catalog has been observed, loss of readiness, identity,
version or listener is a sticky invalidation. Missing descriptors retain conservative
legacy validation; failed stores and missing mid-run indices are not skipped.

For a separately authorized combined acceptance:

1. Start matching CPA and Pacer with a **fresh consumer/listener incarnation** while
   preserving prior DB faults. Confirm runtime catalog and listener both `ready`.
2. Install the durable consumer before checkpoint requests. An unobserved source
   may need one dummy warmup attempt; its accounting is retained but is not included
   in the later selected cut interval.
3. Wait causally for acknowledged usage and actual producer/body closure, then
   observe/persist the explicit **unknown / first_quiescent_cut** lower boundary.
4. Only then run the selected TTL attempts; close streams/bodies and acknowledge
   callbacks. Obtain a known upper cut and select the exact two boundary identities.
5. Assert the selected event IDs/sequences exclude warmup/pre-lower records. Keep
   numeric-time physical carry-in and interval-complete billing membership unknown.

No gap clearing, existing-incarnation healing, arbitrary timestamp selection or
manual readiness flag is valid. The source-only tests use local native ABI fixtures,
real service/file-store loading, controlled channels and dummy credentials; they do
not start an API server, use the active lab, or issue provider requests.
