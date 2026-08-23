# Contributing to Muzak

Thanks for taking the time. This document is short because most of what matters
is already enforced by the test suite.

## Getting set up

Muzak needs Go 1.27. The framework uses generic methods, generalized function
type inference, `encoding/json/v2` and the standard library `uuid` package, none
of which exist in earlier versions.

```bash
git clone https://github.com/muzak-dev/framework
cd framework
go test ./...
```

There is nothing else to install. Muzak has no third-party dependencies, and
that is a property worth keeping: a pull request that adds one needs to make the
case for it in the description.

## Before you open a pull request

```bash
go build ./...
go vet ./...
go test ./... -count=1
go test ./... -race -count=1
golangci-lint run ./...
```

The race detector is not optional. Request-scoped state, the dependency
container, the WebSocket and SSE registries and the rate limit table are all
shared across goroutines by design, and a data race in any of them is a bug that
reaches production as corrupted state rather than as a crash.

## The rules the suite enforces

Some of these will fail your build in ways that are not obvious the first time.

- **Coverage.** The suite sits at 99.0% of statements. A statement that genuinely
  cannot be reached carries a `// coverage:` comment saying why, and a test
  checks that those comments contain a real justification rather than a bare
  marker.
- **No em dash.** The character does not appear anywhere in source, docs or
  output, and a test enforces it.
- **Goroutine leaks.** Request handling, the dependency container and the server
  lifecycle are asserted to leave nothing running.
- **Build-time errors are reported together.** When you add a new class of
  misconfiguration, join it into the existing error rather than returning early,
  so a first run still lists everything wrong at once.

## Writing code

Match the surrounding code. Concretely, in this repository that means:

- **Comments say why, not what.** The reader can see what the line does. What
  they cannot see is the alternative you rejected and the reason.
- **Every exported symbol has a doc comment**, and it explains the decision the
  caller has to make, not just the signature.
- **Errors are sentences.** They start with `muzak: `, they read as prose, and
  they name the thing that is wrong and what to do instead.
- **Nothing derived from an internal failure reaches a client.** Wrap it so it
  reaches the log instead.
- **Safe by default.** A new option that relaxes a bound defaults to the
  conservative value, and its doc comment says what relaxing it costs.

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).
