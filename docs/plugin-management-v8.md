# Explicit scoped v8 plugin management

This is additive host plumbing. It implements no report domain endpoints and
does not migrate, alias or maintain deprecated `/v0/management` features.
The forward-reporting proposal's v0 prefix is superseded by the v8 contract here;
forward route implementations and live acceptance remain separately owned.

## Registration capability and SDK copy set

The only changed SDK source is **`sdk/pluginapi/types.go`**. The native RPC still
uses `management.register` and `management.handle`; schema remains **7**, C ABI
remains **1**. No capability bit or numeric version was added.

`pluginapi.ManagementRegistrationRequest` adds:

```go
PluginManagementBasePath string `json:"PluginManagementBasePath,omitempty"`
```

The host advertises this only for a schema >= 7 plugin with a canonical record ID.
For the record ID `pacer`, the registration request contains:

```json
{
  "BasePath": "/v0/management",
  "ResourceBasePath": "/v0/resource/plugins/pacer",
  "PluginManagementBasePath": "/v8/management/plugins/pacer"
}
```

Other registration fields, including `Plugin`, remain unchanged. Ownership uses
the host's **record ID**, never the plugin's display name or self-declared path.
The old prefixes retain their existing values. The new JSON field is omitted
when unavailable; a newer plugin decoding an older host request sees `""`.
Schema 7 alone does **not** imply host support: the nonempty advertised prefix
is the registration capability probe.

Consumers with a mirrored native type (for example
`SDKManagementRegistrationRequest`) need only mirror the same additive string
field and JSON spelling; they do not need new C exports, RPC methods or changes
to usage/source-proof structures. No consumer SDK files were edited by this task.

## Declaring routes

Use the advertised full prefix explicitly; do not concatenate the legacy BasePath.
For example, a consumer owning a status handler could opt in as follows:

```go
if req.PluginManagementBasePath == "" {
    // Host lacks this capability. Do not register or fall back to a v0 feature.
    return pluginapi.ManagementRegistrationResponse{}, nil
}
return pluginapi.ManagementRegistrationResponse{
    Routes: []pluginapi.ManagementRoute{{
        Method: "GET",
        Path: req.PluginManagementBasePath + "/reports/forward/status",
        Handler: statusHandler,
    }},
}, nil
```

This is a **consumer example, not an endpoint installed by CPA**. After a consumer
actually registers it, a parent-controlled authenticated probe would be:

```http
GET /v8/management/plugins/pacer/reports/forward/status HTTP/1.1
Authorization: Bearer <management-key>
Accept: application/json
```

Before consumer registration the authenticated path returns 404. An existing
schema-7 plugin with only relative/v0 routes acquires no v8 aliases. Missing or
invalid/inference credentials use the existing management middleware's 401/403
semantics; inference credentials do not authorize this route. No probe was run
against a lab or deployed service in this task.

### Literal namespace rules

- Full path `/v8/management/plugins/<own-record-id>/<one-or-more-segments>` only.
  Every ID/segment is nonempty ASCII `[A-Za-z0-9_-]+`; maximum full path 2048 bytes.
- Methods are explicitly uppercase `GET`, `POST`, `PUT`, `PATCH`, `DELETE`.
  No default verb, HEAD/OPTIONS auto-alias, parameter, wildcard, dot, traversal,
  doubled/trailing slash, backslash, query, fragment or percent encoding in paths.
- V8-looking declarations in the `v8` root namespace are handled before legacy
  normalization, including malformed and missing-leading-slash declarations.
  They cannot become `/v0/management/v8/...` by falling through.
- `Menu` must be empty, even on GET. V8 routes cannot become public resources.
  Existing resource registration and `/v0/resource/plugins/<id>/` stay unchanged.
- All existing v8 Gin built-in patterns reserve their matching paths **across
  verbs**. A plugin cannot claim `/plugins/<id>/quota`, even with PATCH, or a
  matching parameter/catch-all route. Fixed Gin routes retain dispatch precedence.
- First/higher-priority valid declaration wins, as with legacy registration.
  A different plugin cannot register paths in another record's namespace.

## Dispatch, limits and headers

The v8 NoRoute branch requires registered/enabled management, a live host and the
same existing availability check and management-key middleware exactly once.
Home mode, disabled/unregistered management and missing/fused/stale routes do not
dispatch. There is no `ServePluginAuthURL` fallback in this branch.

`Host.ServeManagementV8HTTP` is an **authenticated-caller helper**, not standalone
authentication. CPA calls it only after management authorization. It is separate
from the unchanged legacy `ServeManagementHTTP` implementation.

- Request body: at most **64 KiB**, including unknown-length/chunked input; 413
  before plugin invocation on excess. Raw query: at most 64 KiB with strict URL
  parsing. Request read/query/header failures are sanitized 400 responses.
- Only request headers **Content-Type, Accept, If-Match** are copied; values must
  be control-free and at most 8192 bytes each. Authorization, X-Management-Key,
  cookies, inference/API keys, private capabilities and arbitrary headers are
  excluded. Query/body values remain plugin inputs, not authentication sources.
- Response body: at most **8 MiB**, inclusive; oversize returns a sanitized 502
  without partial output. This bounds HTTP output, not an in-process plugin's
  allocation or execution time. No arbitrary plugin execution timeout is added.
- Status zero defaults to 200. Only final 2xx/4xx/5xx statuses are accepted;
  redirects, informational/invalid statuses and a body on 204 are rejected with
  502. Plugin handler errors return a fixed 502 without private error text.
- Response headers are limited to **Content-Type** and **Content-Disposition**.
  Content-Type defaults to application/json and allows application/json,
  application/problem+json, text/csv, application/octet-stream (valid MIME
  parameters permitted). Disposition, if supplied, must be attachment with only
  a nonempty control-free, slash-free filename. Both header values are bounded.
  Cookies, Location, auth challenges, cache overrides and private headers are
  never forwarded. Host authentication/version headers remain host-owned.
- **Cache-Control: no-store** and **X-Content-Type-Options: nosniff** are set for
  dynamic v8 responses and refusals, including middleware errors/404s.
- Schema >= 7 response bodies are **opaque bytes**: no JSON decode/re-encode,
  numeric conversion, HTML escaping or report recomputation. Native download
  JSON such as `{filename,media_type,content}` remains byte-exact. Plugin-supplied
  domain errors retain their status/body if they satisfy this envelope contract.
  Older schemas remain on their existing v0 response rules; no v8 registration.

Plugins still own strict domain input validation, unknown/duplicate field checks,
pins, cursors, authority, calculation and safe error bodies. These generic host
limits do not relax or replace mandatory source-proof/domain validation.

## Verification scope

New tests exercise `Host.RegisterManagementRoutes`, the native RPC adapter with a
local synthetic client, and the real Gin NoRoute/auth middleware using httptest.
They include registration/owner/schema/menu/path/method refusals, reserved Gin
patterns, legacy route/resource behavior, credential isolation, middleware ban
counting, raw JSON/wide-number/export bytes, both size boundaries and sanitized
failures. No standalone public fake handler replaces plugin dispatch.

Source acceptance does not establish deployed native registration, forward live
coverage, physical source proof, migration parity or retirement readiness.
