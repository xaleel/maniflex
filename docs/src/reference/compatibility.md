# Stability & Compatibility

This page is the contract behind the version number: what will not change while
maniflex is v1, what may change, and what happens to something once it is
deprecated.

**It takes effect at `v1.0.0`.** While the project is v0.x, any minor release may
break anything. Nothing below applies retroactively to v0.x.

maniflex has two distinct sets of consumers, and they depend on different things:

| Contract          | Who depends on it                                       | Where it lives                                   |
| ----------------- | ------------------------------------------------------- | ------------------------------------------------ |
| **Go API**        | applications importing `github.com/xaleel/maniflex/...` | exported identifiers                             |
| **HTTP contract** | clients calling the generated API                       | routes, query parameters, envelopes, error codes |

Both are covered. The HTTP contract matters most in practice — a mobile app or a
generated SDK talks to the API, not to the Go types — and it is the one a Go
compatibility promise alone would leave unaddressed.

## The Go API contract

Within v1.x, in a covered module:

- Exported identifiers keep their names, kinds, and signatures.
- Exported struct fields keep their names, types, and meanings.
- **Zero values keep their behaviour.** A `Config` field that means "unlimited"
  when unset does not quietly start meaning "disabled". Most maniflex
  configuration is a zero-value default, so this is load-bearing.
- Sentinel errors (`ErrNotFound`, `ErrIncrementOutOfBounds`, and the rest) keep
  their identity and stay `errors.Is`-comparable.

New exported API may be added in any minor. That has one consequence you must
plan for:

> **Use keyed struct literals.** `maniflex.Config{Port: 8080}`, never
> `maniflex.Config{8080, ...}`. Adding a field to an exported struct is an
> additive change under this policy and it breaks unkeyed literals. Unkeyed
> literals of maniflex structs are not supported.

### Not covered

- Anything under `internal/`, which the Go toolchain already prevents you from
  importing.
- The **text** of error messages. `code` in the error envelope and the sentinel
  error values are the contract; the human-readable string is not. Do not match
  on message prose.
- Log output — format, wording, levels, and which events are logged at all.
- Metric and span names emitted by `pkg/otel`, until a future release freezes
  them explicitly.
- Struct field _order_, which matters only to unkeyed literals, already excluded
  above.
- Unsafe or reflective access to unexported state.

### Extension interfaces are closed

`DBAdapter`, `Tx`, `FileStorage`, `CacheStore`, `KeyProvider`, the `events` and
`jobs` broker and queue interfaces, `auth.Revoker`, the `Locker` interfaces, and
their siblings are covered **for callers**: if you accept or call one, its
existing methods will not change.

They are **closed to outside implementation**. Methods may be added to them in a
minor release, which breaks any type implementing the interface from outside this
repository.

This is a deliberate trade. maniflex is built around pluggable backends, and a
fully frozen `DBAdapter` would mean no adapter could gain a capability until v2 —
so the interface that exists to enable extension would be the one thing blocking
it. If you maintain an out-of-tree adapter, pin the minor version and expect to
add methods when you upgrade; opening an issue about it is the fastest way to get
a compatibility shim considered.

## Deprecation and removal

**Nothing exported is removed during v1.x.**

A Go module's import path encodes its major version, so removing an exported identifier requires a `/v2` path — there
is no mechanism for "removed in v1.4" that does not break every importer. A
policy promising removal after some number of minor releases would be a promise
Go will not let this project keep.

So a deprecation is a signal, not a countdown:

1. The identifier gains a `// Deprecated:` godoc comment naming its replacement,
   or stating plainly that it has none. Editors and linters surface these.
2. The CHANGELOG entry records it.
3. **It keeps working, unchanged, for all of v1.x.** A deprecated field does not
   become a silent no-op — that is a behaviour change, and behaviour changes are
   breaking whether or not the symbol survives.
4. It is removed in v2.0.0, at a new module path, alongside a migration guide.

The practical cost lands on new API rather than old: anything exported in v1 is
permanent, so it is worth being slower to export. Prefer an unexported type with
an exported constructor, and prefer a concrete struct over an interface, unless
there is a reason to widen.

Breaking changes are marked in the CHANGELOG as `**(breaking)**` on the relevant
category, and a behaviour change that keeps every signature intact is marked
`**(behaviour change)**` — the more dangerous of the two, because it compiles.

## The HTTP contract

Within v1.x, for the routes maniflex generates:

- **Route shapes** for every generated operation — collection, item, and the
  mounted sub-resources (`/export`, `/aggregate`, `/{field}/upload-url`,
  `/{id}/restore`, `/{id}/history`, `/{id}/{field}`) — keep their paths and
  methods, under whatever `PathPrefix` you configure.
- **Query parameters** keep their names and semantics: `filter` (and its
  `filter[N]` group form), `sort`, `page`, `limit`, `cursor`, `include`,
  `select`, `format`, `aggregate`, and `q`/`models` on global search. See
  [Querying](../using-the-api/querying.md).
- **Envelopes** keep their shape: `{"data": ...}` on success, `{"meta": ...}` on
  lists, `{"error": {"code", "message", "details"}}` on failure, and `details` is
  an array wherever it appears. See [Response Envelope](../using-the-api/responses.md).
- **Error codes** are permanent. A code is never removed, and never repurposed to
  mean something else.
- **Status codes** for each generated operation stay as documented.
- The **DDL contract** in [Field Schema & Nullability](../defining-your-api/schema.md)
  and the **identity contract** in [Record Identity](../defining-your-api/identity.md)
  keep their existing "out of contract for v1" carve-outs.

### What may still change, and what your client must tolerate

Minor releases may add. Specifically, they may add response fields, query
parameters, error codes, routes, and headers.

That is only safe if clients are written for it, so it is part of the contract in
the other direction — **the API is additive, and a conforming client must:**

- ignore JSON fields it does not recognise, rather than rejecting the response;
- treat an unrecognised `error.code` as a generic failure of its HTTP status
  class, rather than crashing;
- not depend on JSON key ordering.

A strict-by-default generated SDK, or a deserialiser configured to error on
unknown fields, will break on a minor release. That is the client's
configuration, not a breach of this policy.

Also outside the contract: the exact prose of `error.message`, the byte-level
output of the OpenAPI document (its _semantic_ content is covered), and the
`admin` panel's HTML, which is a UI rather than an API.

### Your API's own evolution is yours

This page covers what maniflex generates, not what your application exposes. If
you rename a model field, maniflex will faithfully rename the JSON key and break
your clients — the framework has no field-deprecation or API-versioning mechanism
of its own, and shipping one is not planned for v1.

Until it exists, the tools are ordinary ones: keep the old column and populate
both during a transition, add a computed field with
`Server.AddComputedField` to serve the old name from the new data, or mount a
versioned `PathPrefix` per API generation.

## Minimum versions

**Go: 1.25.12.** Every module declares it, and CI builds each release against
both that exact version and current stable. The minimum may be raised in a minor
release — this is the convention across the Go ecosystem, including the standard
library's own support window — but never in a patch release.

The declaration is patch-precise (`go 1.25.12`, not `go 1.25`), so a consumer on
an earlier 1.25 patch cannot build maniflex at all. That is stricter than most Go
libraries and is worth knowing before you pin a toolchain.

**Databases: what CI proves.** Continuous integration runs the full end-to-end
suite against PostgreSQL 17 and against `modernc.org/sqlite`, the pure-Go driver
used by `db/sqlite`. Other PostgreSQL versions are widely expected to work — the
adapter uses `lib/pq` and no version-gated syntax — but they are not tested here,
so this page does not promise them.

## Security support

Security fixes land on the current minor release first. Once `v2.0.0` ships, the
final v1 minor continues to receive security fixes for **12 months**. Everything
else is fixed forward.

Report vulnerabilities privately — see [SECURITY.md][security] — not as public
issues.

## Getting to v1.0.0

The stable tag is preceded by at least one release candidate (`v1.0.0-rc.1`,
`rc.2`, …). Release candidates carry no compatibility promise; their purpose is
to find the breaks while breaking is still free.

Covered modules are released in lockstep at a single version, so `v1.2.0` of
core and `v1.2.0` of `db/postgres` are always built and tested together. Mixing
versions across covered modules is unsupported even though Go permits it.

## Enforcement

| Guarantee                                       | Enforced by                                                                      |
| ----------------------------------------------- | -------------------------------------------------------------------------------- |
| Documented Go examples compile                  | `doccheck`, over every fence in `docs/src`, plus anchored includes of real files |
| The README quickstart compiles                  | `examples/quickstart` plus `TestREADMEQuickstartMatchesCompiledExample`          |
| Every mounted route is in the OpenAPI spec      | `TestOpenAPIRouteParity`                                                         |
| The DDL contract holds                          | `tests/e2e/schema_contract_test.go`                                              |
| Modules build from a clean consumer environment | release smoke tests in CI                                                        |

`doccheck` parses Go fences and cannot type-check them, so a fence naming a field
that no longer exists still passes — only compiled examples catch that. This is
why important examples live in `.go` files.

[security]: https://github.com/xaleel/maniflex/blob/main/SECURITY.md
