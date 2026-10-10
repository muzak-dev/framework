# Benchmarks

Measured numbers for Muzak's routing, binding, encoding and logging, plus the
baselines that make them mean something.

## Methodology

```
go test ./... -p 1 -bench=. -benchmem -run=XXX -benchtime=500ms -count=5
```

Each benchmark ran five times, one package at a time (`-p 1`, so that no two
packages' benchmarks share the processor), and the tables report the median
ns/op and the bytes and allocations per operation. Every case drives the real
code path: requests go through `App.ServeHTTP`, so routing, binding, dependency
resolution, response encoding and the middleware chain are all included unless
a row says otherwise.

Responses are written to a `ResponseWriter` that discards its output, so the
numbers measure the framework rather than the recorder.

| | |
|---|---|
| Machine | Apple M1 Pro, 10 cores |
| OS / arch | darwin / arm64 |
| Go | 1.27.0 |
| Framework | 0.3.0, measured 2026-10-10 |
| Command | `go test ./... -p 1 -bench=. -benchmem -run=XXX -benchtime=500ms -count=5` |

The machine was not idle: other work kept its load average between 6 and 13
while these ran, and a third of the rows varied by more than 10% from one run
to the next. Read the nanoseconds as an upper bound that a quiet machine
improves on, and the comparisons between rows, which were measured under the
same conditions, as what holds. Bytes and allocations per operation do not
depend on the load and are exact.

## Routing

The tree is measured against a route set of the size a real service registers
(17 patterns across static, parameter and wildcard segments), so lookup is not
being timed against a single entry.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkLookupStatic` | 75.3 | 0 | 0 |
| `BenchmarkLookupParam` | 92.4 | 0 | 0 |
| `BenchmarkLookupWildcard` | 27.0 | 0 | 0 |
| `BenchmarkLookupMiss` | 49.7 | 0 | 0 |

Matching allocates nothing. That is the whole point of matching one path
segment per edge rather than one byte: a captured parameter is a sub-slice of
the request path, and captures are appended to a `Params` whose backing arrays
are reused across requests. A miss is cheaper than a hit because it stops at
the first segment with no matching edge.

## End-to-end request handling

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkBaselineServeMux` | 628.4 | 312 | 9 |
| `BenchmarkRouteParamBare` | 786.0 | 680 | 11 |
| `BenchmarkBindEmpty` | 1284 | 1120 | 17 |
| `BenchmarkRouteParam` | 1272 | 1176 | 19 |
| `BenchmarkRouteStatic` | 1308 | 1120 | 17 |
| `BenchmarkDependencyResolution` | 1307 | 1152 | 19 |
| `BenchmarkBindPathQueryHeader` | 1661 | 1672 | 24 |
| `BenchmarkRouteNotFound` | 2340 | 1400 | 27 |
| `BenchmarkErrorResponse` | 3021 | 1888 | 32 |
| `BenchmarkBindJSONBody` | 3406 | 7081 | 38 |

Validation is measured separately below, because it is opt-in per model.

The two rows worth reading together are the first two.

`BenchmarkBaselineServeMux` is a bare `net/http` handler registered on
`http.ServeMux` doing exactly the same work by hand: match a path parameter,
build the output struct, encode it with `encoding/json/v2`, set Content-Type
and Content-Length, write. `BenchmarkRouteParamBare` is the same route through
Muzak with the optional middleware removed.

The hand-written handler is faster: **628 ns against 786 ns, and 9
allocations against 11.** A memory profile of the two says where the
difference goes, and it is almost entirely one thing. Muzak copies the request
once, with `WithContext`, to install the holder that `RouteFromContext`, the
access log, the server span and the request observer read the matched route
from: 320 bytes for the copy, 48 for the context value and 8 for the holder,
376 bytes in all against a difference of 368. The response writer Muzak wraps
around the one net/http provides is another 64 bytes, and binding the path
parameter into the input costs about what `ServeMux`'s own pattern match and
the hand-written buffer do. The typed layer itself is close to free; what the
bare path pays for is the instrumentation hook every request carries whether
or not anything reads it, which is the obvious place to look for the next
saving.

> **Earlier versions of this document reported 545.6 ns and 8 allocations for
> the bare path, and said it beat `ServeMux`.** That was true when it was
> measured, before 0.2.5 added the route holder and the request copy above;
> 0.2.9 measures the same 11 allocations as 0.3.0, so the claim had been out of
> date for several releases before it was corrected here. An even earlier
> figure of 451.8 ns and 6 allocations was wrong outright: the benchmark
> rebuilt an application that was already mounted, every timed request was
> served as a 500, and it measured the error path. Every benchmark now asserts
> the status it is meant to serve before its timed loop begins, so one that
> stops measuring what it claims fails instead of reporting a flattering number.

The remaining rows carry the default middleware chain, which is what accounts
for the step from about 790 ns to about 1300 ns. Two things dominate it:

- **Request identifiers.** Every request gets a version 7 UUID, generated from
  `crypto/rand` and formatted as a string, so that every error response and
  every log line can be correlated. Remove it with a custom middleware chain if
  a request never needs to be traced.
- **Security headers.** Several header writes per response.

Both are on by default because a service that has to add them later usually
does not. `AppOptions.DisableSecurityHeaders` and a hand-built middleware chain
remove them when the cost matters more than the guarantee.

`BenchmarkBindJSONBody` allocates the most because it constructs a fresh
`http.Request` per iteration to supply a fresh body reader; most of that figure
is the request, not the decoding.

## Binding

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkBindPlanCompilation` | 1346 | 1544 | 14 |

This is the reflection Muzak performs to compile a binding plan: walking the
input type, classifying each field, resolving a setter per field, and checking
declared path parameters against the route template.

It runs **once per route, at start-up**. An application with 200 routes spends
about 0.3 ms of its start-up compiling plans, and no request afterwards
inspects a type. The comparison that matters is against a reflection-per-request
design, where this ~1.3 microseconds would land on every single request
instead of once.

## JSON

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkJSONMarshal` | 291.1 | 64 | 1 |
| `BenchmarkJSONUnmarshal` | 449.4 | 160 | 4 |

Encoding a four-field response into a pooled buffer costs one allocation, which
is the buffer growing to fit. Decoding runs with `RejectUnknownMembers`, so the
number includes the strictness Muzak applies by default rather than measuring
a laxer configuration.

Responses are encoded fully before anything is written. That costs a buffer,
and buys the guarantee that a value which fails to encode part way through
produces a clean error response instead of a truncated body.

## Validation

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkValidation/skipped` | 3423 | 7089 | 37 |
| `BenchmarkValidation/validated` | 4777 | 7810 | 49 |

The pair measures the same request through the same model, once with the rules
applied and once with `SkipValidation()`, so the difference is what the rules
themselves cost: **about 1.4 microseconds and 12 allocations** for a model
declaring eight rule sets, including an email parse and two regular
expressions. The nanoseconds of this pair varied more than most from run to
run; the allocations did not.

When this section was first written the rules cost 1.7 microseconds and 46
allocations, and two changes the profiler pointed at took that down to 11
allocations:

- **Rules carry their parameters as data.** Every built-in rule used to be a
  closure, allocated afresh each time a model declared its rules, which is once
  per request. A rule now records what it needs in the step and a dispatch
  function reads it, so `MinLen(12)` appends a value to a slice rather than
  building a function. Constraints are derived the same way, which removed a
  second closure per rule.
- **Rule sets are recycled.** A model declares the same shape on every request,
  so the `Validation` keeps the rule sets it has already built and hands them
  back after a reset. Their step slices keep their capacity, which is what makes
  the first change pay: after the first request through a route, redeclaring the
  rules allocates nothing at all.

A third change was smaller but worth recording: the string and numeric paths are
written out rather than reached through a function value. Passing the value
pointer into an indirect call forces the compiler to assume it escapes, and to
heap-allocate the value on every request.

What remains is mostly the element rule sets, which a model builds inline
(`Each(validate.String().MaxLen(20))`) and which therefore cannot be recycled
without changing the API. `SkipValidation()` stays the escape hatch for a hot
route that validates elsewhere.

## Observability

The same route with tracing and the request observer off, then on, and a
request that matches no route with both off:

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkObservabilityOff` | 1498 | 1520 | 21 |
| `BenchmarkObservabilityOn` | 2401 | 2600 | 31 |
| `BenchmarkObservabilityOffNotFound` | 2423 | 1424 | 28 |

Turning tracing and an observer on costs **about 0.9 microseconds and 10
allocations** per request: the span and the request copy that carries it to
the handler, the span's attributes, the client address among them, parsed
from the connection, and the trace and span identifiers written as text into
the access log line. Left off, nothing is installed and nothing is allocated
for it, which is what lets every application carry the hook.

## WebSockets

The frame codec, with nothing else in the way:

| Benchmark | ns/op | throughput | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `BenchmarkAppendHeader` | 2.9 | | 0 | 0 |
| `BenchmarkReadHeader` | 31.4 | | 32 | 2 |
| `BenchmarkMask/7` | 7.7 | 0.9 GB/s | 0 | 0 |
| `BenchmarkMask/64` | 5.5 | 11.6 GB/s | 0 | 0 |
| `BenchmarkMask/1024` | 83.7 | 12.2 GB/s | 0 | 0 |
| `BenchmarkMask/65536` | 4922 | 13.3 GB/s | 0 | 0 |

Masking runs eight bytes at a time from a key rotated to the position within
the message, so a payload that arrives in pieces costs the same as one that
arrives whole and neither allocates. Below eight bytes the loop is all tail,
which is the only case where the per-byte rate drops.

Writing one message from a real route's handler, with the context it was
given, over a real connection:

| Benchmark | ns/op | throughput | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `BenchmarkWebSocketHandlerWrite/64B` | 4272 | 15 MB/s | 0 | 0 |
| `BenchmarkWebSocketHandlerWrite/1KiB` | 5018 | 204 MB/s | 0 | 0 |
| `BenchmarkWebSocketHandlerWrite/64KiB` | 14650 | 4.5 GB/s | 0 | 0 |

A message a handler sends with `ctx.Context()` allocates nothing at all. The
header is built into a buffer the connection owns, a payload that fits it is
copied in so the frame goes out in one write, and one larger than it is written
straight from the caller's own memory. The write deadline is set on the socket,
and the connection watches the request's own context once, for its whole life.
Nothing is retained, so the caller may reuse its slice the moment the call
returns. The nanoseconds here include the network round trip of the test
server, which is why they are the larger figures on this page.

The same write on a bare connection with no transport behind it, so that the
number is the framing rather than the network:

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkWebSocketFrameWrite/background/64B` | 274.2 | 200 | 5 |
| `BenchmarkWebSocketFrameWrite/request/64B` | 291.0 | 200 | 5 |
| `BenchmarkWebSocketFrameWrite/request/1KiB` | 288.6 | 200 | 5 |
| `BenchmarkWebSocketFrameWrite/request/64KiB` | 283.3 | 200 | 5 |
| `BenchmarkWebSocketFrameWrite/derived/64B` | 488.3 | 432 | 11 |

A transport with no deadlines of its own, which is what this bare connection
and the body of a client's switched connection are, cannot be interrupted, so
each write arms a timer that ends the connection if the write outlives its
`WriteTimeout`. Those are the five allocations on every row, and why 64 KiB
costs no more than 64 bytes. The `derived` row is the same write given a
context the connection is not already watching, such as one carrying a
deadline for that message alone; the extra six allocations are
`context.AfterFunc` arranging for the cancellation to interrupt the write.

End to end over an in-memory pipe, which is synchronous, so each row includes
the peer reading what was written and the scheduler handing control back:

| Benchmark | ns/op | throughput | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `BenchmarkWebSocketWrite/server/64B` | 1777 | 36 MB/s | 352 | 7 |
| `BenchmarkWebSocketWrite/server/64KiB` | 10850 | 6.0 GB/s | 114976 | 8 |
| `BenchmarkWebSocketWrite/client/64KiB` | 33870 | 1.9 GB/s | 114976 | 8 |
| `BenchmarkWebSocketRead/64B` | 1896 | 34 MB/s | 352 | 7 |
| `BenchmarkWebSocketRead/64KiB` | 37850 | 1.7 GB/s | 114976 | 8 |
| `BenchmarkWebSocketRoundTrip` | 3721 | | 600 | 15 |

The gap between the server and the client at 64 KiB is masking: a server sends
its payload untouched, while a client has to mask a copy of it, which the
`BenchmarkMask` rows above price at about 4.9 microseconds for that size. The
bytes per operation are mostly the message itself, allocated once per read and
handed to the caller to keep; there is no shared buffer to be surprised by.
Four of the allocations on each row are `net.Pipe`'s own: it implements a
deadline with a timer each time one is set, which a TCP socket does not.

`BenchmarkWebSocketRoundTrip` is one message each way, which is what a request
and reply conversation costs. The handshake adds `BenchmarkWebSocketAcceptKey`
at 153 ns and 3 allocations, once per connection.

## Server-sent events

One event on its way out, measured against a discarding response so that the
number is the engine rather than the network:

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkSSESend` | 284.8 | 32 | 2 |
| `BenchmarkSSESendEvent` | 316.9 | 32 | 2 |
| `BenchmarkSSESendText` | 123.1 | 0 | 0 |
| `BenchmarkSSEEncodeEvent` | 27.3 | 0 | 0 |

The framing costs nothing: an event of text goes out with no allocation at all,
and the two on the typed rows are what handing a value to the encoder takes.
Both buffers belong to the stream and are reused for its whole life, and a
single event that grows one of them past 64 KiB releases it rather than pinning
that memory for the hours a stream may last.

Reading one event, which is the client half of the same engine:

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkSSEParseEvent` | 607.3 | 397 | 11 |

Lines are taken from what the buffer already holds rather than a byte at a
time; the allocations are the strings an event is delivered as.

## Rate limiting

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkRateLimit/unlimited` | 1011 | 993 | 16 |
| `BenchmarkRateLimit/one quota` | 1468 | 1160 | 26 |
| `BenchmarkRateLimit/three quotas` | 1667 | 1216 | 28 |

The three rows are the same request through the same route, once unlimited and
twice with a policy, so the difference is what limiting costs: **about 0.5
microseconds and 10 allocations** for the first quota, and roughly 0.1
microseconds and one allocation for each one after it.

The fixed part is resolving the client address, building the key from it and
writing the four `RateLimit` response headers, which is why the first quota
costs several times what the second does. The per-quota part is one call into
the storage, which for the built-in memory storage is a mutex, a map lookup and
a heap fixup. A storage that talks to Redis replaces that with a round trip per
quota, which is the reason a policy is worth keeping to a handful of windows.

Two things were done about the fixed cost, both of which the profiler pointed
at. The header names are canonicalised once at start-up rather than on every
`Set`, because `RateLimit-Limit` is not the form `net/http` stores names in and
being rewritten costs an allocation each time. And the address is resolved
without allocating at all: the forwarding header is walked from the right
without being split, and the result is a `netip.Addr` rather than a parsed
structure.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkClientIP/peer` | 34.8 | 0 | 0 |
| `BenchmarkClientIP/forwarded` | 157.8 | 0 | 0 |

The forwarded case walks three hops of `X-Forwarded-For` behind a trusted
prefix, which is what a service behind a load balancer and a CDN receives.

## Logging

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkLoggerConsole` | 719.1 | 0 | 0 |
| `BenchmarkLoggerJSON` | 1512 | 16 | 2 |

Both measure a scoped logger writing a message with two attributes, including
the redaction check on every key. The console handler is the faster of the two
because it formats into a pooled byte slice and writes once, and because
attributes supplied through `With` are rendered when the child logger is built
rather than on every record. These two varied the most of any row on this page
from run to run; the allocations did not.

Records below the configured level cost a single comparison: `Enabled` is
checked before any formatting work happens. `LogFormatNone` discards everything
without formatting at all, which is what the test suite uses.

## Reproducing

```
go test ./... -p 1 -bench=. -benchmem -run=XXX -benchtime=500ms -count=5
```

Absolute numbers will differ by machine, and on a busy one. The relationships
should hold: routing allocates nothing, the bare framework path costs a little
more than `http.ServeMux` at the same work, almost all of it the route holder,
a handler's WebSocket write allocates nothing, observability costs nothing when
it is off, and plan compilation happens once per route rather than once per
request.
