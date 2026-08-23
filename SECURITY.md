# Security Policy

## Reporting a vulnerability

Please do not open a public issue.

Report privately through GitHub's [private vulnerability
reporting](https://github.com/muzak-dev/framework/security/advisories/new) on
this repository. That opens a draft advisory only the maintainers can see.

Include what you need to make the problem reproducible: the version or commit,
the configuration involved, and the request or sequence that triggers it. A
failing test against the framework is the most useful thing you can send.

You should get an acknowledgement within a few days. Once a fix is ready it ships
with an advisory crediting you, unless you would rather not be named.

## Scope

In scope: anything in this repository that lets a client reach past a bound the
framework claims to hold. The bounds are enumerated in the README and in
[Safe Defaults](https://muzak.dev/docs/security/safe-defaults), and the ones most
worth attacking are:

- request, form and file size limits, and the point at which they are enforced
- the WebSocket bounds: message size, frame count, connection counts per process
  and per address, and the origin check
- the event stream bounds: write timeouts, stream counts, and the refusal of a
  line break in an event name or identifier
- rate limit accounting: counter growth, key handling, and the storage failure path
- client address resolution and which forwarding headers are believed
- the boundary between an error's client-visible message and its logged cause
- request binding: whether a crafted body can reach a field bound from the path,
  the query string, a header or a cookie

Out of scope: a configuration that deliberately relaxes a bound and then suffers
the consequence the doc comment describes. `InsecureSkipOriginCheck` accepting a
cross-origin handshake is the documented behaviour of that option, not a
vulnerability.

## Past review

[docs/security-review.md](docs/security-review.md) records an adversarial review
of every subsystem, and the findings it lists are fixed. It is kept in the
repository because the reasoning behind each bound is more useful than the
finding was.
