# Benchmarks

Measured numbers for Muzak's routing, binding, encoding and logging, plus the
baselines that make them mean something.

## Methodology

```
go test ./... -bench=. -benchmem -run=XXX -benchtime=500ms -count=3
```

Each benchmark ran three times; the tables report the median ns/op and the
bytes and allocations per operation. Every case drives the real code path:
requests go through `App.ServeHTTP`, so routing, binding, dependency
resolution, response encoding and the middleware chain are all included unless
a row says otherwise.

Responses are written to a `ResponseWriter` that discards its output, so the
numbers measure the framework rather than the recorder.

| | |
|---|---|
| Machine | Apple M1 Pro, 10 cores |
| OS / arch | darwin / arm64 |
| Go | 1.27.0 |
| Command | `go test ./... -bench=. -benchmem -run=XXX -benchtime=500ms -count=3` |

## Routing

The tree is measured against a route set of the size a real service registers
(17 patterns across static, parameter and wildcard segments), so lookup is not
being timed against a single entry.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkLookupStatic` | 64.4 | 0 | 0 |
| `BenchmarkLookupParam` | 80.5 | 0 | 0 |
| `BenchmarkLookupWildcard` | 19.6 | 0 | 0 |
| `BenchmarkLookupMiss` | 41.8 | 0 | 0 |

Matching allocates nothing. That is the whole point of matching one path
segment per edge rather than one byte: a captured parameter is a sub-slice of
the request path, and captures are appended to a `Params` whose backing arrays
are reused across requests. A miss is cheaper than a hit because it stops at
the first segment with no matching edge.

## End-to-end request handling

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkRouteParamBare` | 545.6 | 264 | 8 |
| `BenchmarkBaselineServeMux` | 608.1 | 312 | 9 |
| `BenchmarkBindEmpty` | 1069 | 731 | 14 |
| `BenchmarkRouteStatic` | 1116 | 730 | 14 |
| `BenchmarkRouteParam` | 1141 | 761 | 16 |
| `BenchmarkBindPathQueryHeader` | 1403 | 1241 | 20 |
| `BenchmarkRouteNotFound` | 1467 | 933 | 22 |
| `BenchmarkErrorResponse` | 1874 | 1402 | 26 |
| `BenchmarkBindJSONBody` | 3063 | 6565 | 33 |
| `BenchmarkDependencyResolution` | 1091 | 747 | 15 |

Validation is measured separately below, because it is opt-in per model.

The two rows worth reading together are the first two.

`BenchmarkBaselineServeMux` is a bare `net/http` handler registered on
`http.ServeMux` doing exactly the same work by hand: match a path parameter,
build the output struct, encode it with `encoding/json/v2`, set Content-Type
and Content-Length, write. `BenchmarkRouteParamBare` is the same route through
Muzak with the optional middleware removed.

Muzak is faster, though not by much: **545.6 ns against 608.1 ns, and 8
allocations against 9.** The typed layer is not overhead paid on top of the
standard library so much as a replacement for work the standard library was
already doing: the radix tree beats `ServeMux`'s pattern matching, and the
binding plan turns what would be per-request reflection into a precompiled list
of setters. The honest summary is that you get the typed API for free, not that
you get a large speed-up with it.

> **An earlier version of this document reported 451.8 ns and 6 allocations for
> the bare path.** That figure was wrong. The benchmark stripped the middleware
> after the application had been built and reset the build latch to reassemble
> the chain, which made the next request rebuild an application that was already
> mounted. The rebuild failed, and every timed request was served as a 500. The
> benchmark was measuring the error path, which skips exactly the work being
> measured. Every benchmark now asserts the status it is meant to serve before
> its timed loop begins, so a benchmark that stops measuring what it claims
> fails instead of reporting a flattering number.

The remaining rows carry the default middleware chain, which is what accounts
for the step from ~546 ns to ~1141 ns. Two things dominate it:

- **Request identifiers.** Every request gets a version 7 UUID, generated from
  `crypto/rand` and formatted as a string. That is roughly 4 allocations and a
  few hundred nanoseconds, spent so that every error response and every log
  line can be correlated. Remove it with a custom middleware chain if a request
  never needs to be traced.
- **Security headers.** Three header writes per response.

Both are on by default because a service that has to add them later usually
does not. `AppOptions.DisableSecurityHeaders` and a hand-built middleware chain
remove them when the cost matters more than the guarantee.

`BenchmarkBindJSONBody` allocates the most because it constructs a fresh
`http.Request` per iteration to supply a fresh body reader; roughly 5 KB of
that figure is the request, not the decoding.

## Binding

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkBindPlanCompilation` | 992.0 | 1216 | 11 |

This is the reflection Muzak performs to compile a binding plan: walking the
input type, classifying each field, resolving a setter per field, and checking
declared path parameters against the route template.

It runs **once per route, at start-up**. An application with 200 routes spends
about 0.2 ms of its start-up compiling plans, and no request afterwards
inspects a type. The comparison that matters is against a reflection-per-request
design, where this ~992 ns would land on every single request instead of once.

## JSON

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkJSONMarshal` | 255.6 | 64 | 1 |
| `BenchmarkJSONUnmarshal` | 412.6 | 160 | 4 |

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
| `BenchmarkValidation/skipped` | 3303 | 6576 | 31 |
| `BenchmarkValidation/validated` | 4075 | 7161 | 42 |

The pair measures the same request through the same model, once with the rules
applied and once with `SkipValidation()`, so the difference is what the rules
themselves cost: **about 0.77 microseconds and 11 allocations** for a model
declaring eight rule sets, including an email parse and two regular expressions.

That is down from 1.7 microseconds and 46 allocations, a 76% reduction in
allocations, and it came from two changes the profiler pointed at rather than
from guesswork:

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

## WebSockets

The frame codec, with nothing else in the way:

| Benchmark | ns/op | throughput | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `BenchmarkAppendHeader` | 2.8 | | 0 | 0 |
| `BenchmarkReadHeader` | 30.2 | | 32 | 2 |
| `BenchmarkMask/7` | 7.5 | 1.3 GB/s | 0 | 0 |
| `BenchmarkMask/64` | 5.4 | 11.9 GB/s | 0 | 0 |
| `BenchmarkMask/1024` | 80.5 | 12.7 GB/s | 0 | 0 |
| `BenchmarkMask/65536` | 4824 | 13.5 GB/s | 0 | 0 |

Masking runs eight bytes at a time from a key rotated to the position within
the message, so a payload that arrives in pieces costs the same as one that
arrives whole and neither allocates. Below eight bytes the loop is all tail,
which is the only case where the per-byte rate drops.

Writing one message, measured with no transport behind the connection so that
the number is the framing rather than the network:

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkWebSocketFrameWrite/request/64B` | 102.9 | 0 | 0 |
| `BenchmarkWebSocketFrameWrite/request/1KiB` | 115.7 | 0 | 0 |
| `BenchmarkWebSocketFrameWrite/request/64KiB` | 103.3 | 0 | 0 |
| `BenchmarkWebSocketFrameWrite/derived/64B` | 254.6 | 144 | 3 |

A message a handler sends with `ctx.Context()` allocates nothing at all. The
header is built into a buffer the connection owns, a payload that fits it is
copied in so the frame goes out in one write, and one larger than it is written
straight from the caller's own memory, which is why 64 KiB costs no more than
64 bytes here. Nothing is retained, so the caller may reuse its slice the
moment the call returns.

The `derived` row is the same write given a context the connection is not
already watching, such as one carrying a deadline for that message alone. Those
three allocations are `context.AfterFunc` arranging for the cancellation to
interrupt the write. A connection watches the request's own context once, for
its whole life, which is what keeps that cost off every message a handler sends
with it.

End to end over an in-memory pipe, which is synchronous, so each row includes
the peer reading what was written and the scheduler handing control back:

| Benchmark | ns/op | throughput | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `BenchmarkWebSocketWrite/server/64B` | 1246 | 51 MB/s | 224 | 5 |
| `BenchmarkWebSocketWrite/server/64KiB` | 8731 | 7.2 GB/s | 65696 | 5 |
| `BenchmarkWebSocketWrite/client/64KiB` | 30398 | 2.1 GB/s | 65696 | 5 |
| `BenchmarkWebSocketRead/64B` | 1354 | 47 MB/s | 224 | 5 |
| `BenchmarkWebSocketRead/64KiB` | 30427 | 2.1 GB/s | 65696 | 5 |
| `BenchmarkWebSocketRoundTrip` | 2722 | | 344 | 11 |

The gap between the server and the client at 64 KiB is masking: a server sends
its payload untouched, while a client has to mask a copy of it, which the
`BenchmarkMask` rows above price at about 4.8 microseconds for that size. The
bytes per operation are the message itself, allocated once per read and handed
to the caller to keep; there is no shared buffer to be surprised by.

`BenchmarkWebSocketRoundTrip` is one message each way, which is what a request
and reply conversation costs. The handshake adds `BenchmarkWebSocketAcceptKey`
at 147 ns and 3 allocations, once per connection.

## Server-sent events

One event on its way out, measured against a discarding response so that the
number is the engine rather than the network:

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkSSESend` | 262.3 | 32 | 2 |
| `BenchmarkSSESendEvent` | 280.4 | 32 | 2 |
| `BenchmarkSSESendText` | 107.0 | 0 | 0 |
| `BenchmarkSSEEncodeEvent` | 22.0 | 0 | 0 |

The framing costs nothing: an event of text goes out with no allocation at all,
and the two on the typed rows are what handing a value to the encoder takes.
Both buffers belong to the stream and are reused for its whole life, and a
single event that grows one of them past 64 KiB releases it rather than pinning
that memory for the hours a stream may last.

Reading one event, which is the client half of the same engine:

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkSSEParseEvent` | 464.0 | 397 | 11 |

Lines are taken from what the buffer already holds rather than a byte at a
time; the allocations are the strings an event is delivered as.

## Rate limiting

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkRateLimit/unlimited` | 878.5 | 603 | 13 |
| `BenchmarkRateLimit/one quota` | 1275 | 771 | 23 |
| `BenchmarkRateLimit/three quotas` | 1469 | 827 | 25 |

The three rows are the same request through the same route, once unlimited and
twice with a policy, so the difference is what limiting costs: **about 0.4
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
| `BenchmarkClientIP/peer` | 32.02 | 0 | 0 |
| `BenchmarkClientIP/forwarded` | 147.9 | 0 | 0 |

The forwarded case walks three hops of `X-Forwarded-For` behind a trusted
prefix, which is what a service behind a load balancer and a CDN receives.

## Logging

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `BenchmarkLoggerConsole` | 467.4 | 24 | 2 |
| `BenchmarkLoggerJSON` | 979.1 | 64 | 7 |

Both measure a scoped logger writing a message with two attributes, including
the redaction check on every key. The console handler is the faster of the two
because it formats into a pooled byte slice and writes once, and because
attributes supplied through `With` are rendered when the child logger is built
rather than on every record.

Records below the configured level cost a single comparison: `Enabled` is
checked before any formatting work happens. `LogFormatNone` discards everything
without formatting at all, which is what the test suite uses.

## Reproducing

```
go test ./... -bench=. -benchmem -run=XXX -benchtime=500ms -count=3
```

Absolute numbers will differ by machine. The relationships should hold: routing
allocates nothing, the bare framework path beats `http.ServeMux` at the same
work, and plan compilation happens once per route rather than once per request.
