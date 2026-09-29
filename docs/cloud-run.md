# Running Muzak on Cloud Run

What follows was measured, not read from documentation: a test application built from the framework was deployed to Cloud Run (us-central1, private services, minimum instances 0) on 2026-09-29, once on an HTTP/1 container port and once on an `h2c` container port, and driven from outside with real clients. Each statement names what was observed. Where something was not established, it says so.

## Client addresses: trust the proxy or every client is one client

The address the application sees as the peer is a link-local one, `169.254.169.126`, for every request. It is Cloud Run's proxy, not the client. With the default `ClientIPOptions`, which trusts no proxy, `Context.ClientIP()` is therefore that address for every user, and everything keyed on it (a rate limit, `MaxConnectionsPerIP`, `MaxStreamsPerIP`) treats all users as one. Set the range:

```go
muzak.AppOptions{
	ClientIP: muzak.ClientIPOptions{TrustedProxies: []string{"169.254.0.0/16"}},
}
```

With it set, `ClientIP()` was the caller's real address on every request, including ones that arrived with a spoofed `X-Forwarded-For` of one, two or three invented addresses in front of the real one: the proxy appends the real address last, and the framework reads from the right, so the invented ones were ignored. `Forwarded: for="..."` carries the real address too.

## HTTP/1 or h2c on the container port

Cloud Run speaks HTTP/1 to the container unless the port is named `h2c`. The two behaved differently in ways that matter:

| | HTTP/1 port | `h2c` port |
|---|---|---|
| Request body | held by the platform until it is complete; over 32 MiB it is refused by the platform with an HTML 413 before the application sees it | streamed; 33 MiB reached the handler and was answered by it |
| Body over the route's `MaxBodySize`, declared in `Content-Length` | refused by the framework in 390 ms for 64 KiB bodies, but a large declared body is held by the platform first: with `Expect: 100-continue` the platform answered `100 Continue` itself within 50 ms and then waited for the whole body, so the framework's early refusal could not spare the upload | refused by the framework after 196 KB had arrived, in 394 ms |
| Client leaves mid-request | the handler kept running: still running 45 seconds after the client disconnected | the handler's context was cancelled 400 ms after |
| WebSocket | works | the platform refused the handshake with 502 ("protocol error") |

So: serve an API that has no WebSocket routes on an `h2c` port, and set `ServerOptions.UnencryptedHTTP2` so the server accepts it. Keep WebSocket routes on an HTTP/1 port, which means a separate service if one application needs both.

```go
muzak.AppOptions{
	ServerOptions: muzak.ServerOptions{UnencryptedHTTP2: true},
}
```

On an HTTP/1 port, do work that should stop when its client leaves in steps that check a deadline of their own, since the handler's context is not cancelled by the disconnect.

## Timeouts

The framework's default `WriteTimeout` is 30 seconds and Cloud Run's request timeout is longer (300 seconds by default, up to 3,600). A non-streaming handler that runs past 30 seconds is ended by the server, and the client saw a 503 from the platform after 60 seconds in the test. Raise `ServerOptions.WriteTimeout` for a route that takes that long, or stream. WebSocket and event-stream routes lift the deadline themselves.

## Streams

- **Event streams** arrived incrementally through the platform: a producer sending every 500 ms was received at gaps of 495 to 506 ms, so nothing buffers them.
- **Keepalive** comments keep an idle stream open. The test found the keepalive arriving at 15, 45 and 60 seconds when asked for every 15, a bug fixed in this release (it skipped a tick whenever its own previous write had landed just after the one before). After the fix it arrives every interval.
- **`MaxLifetime`** ended a stream cleanly after 20 seconds, and a reconnect with `Last-Event-ID` resumed from the next event on the same instance.
- **WebSockets** on the HTTP/1 port survived 100 seconds with no application traffic, with the default keepalive (a ping every 30 seconds) and for a handler that only reads. A message over the read limit ended the connection; the client saw the connection lost (1006) more often than the 1009 the server sends.

## Deployments

A connection open when a new revision starts serving is not closed by the rollout: a WebSocket and an event stream were both still open five minutes after the new revision took over. They are closed when Cloud Run scales the old instance in, and then the framework sends its going-away close (1001) or ends the stream cleanly. A connection that must not outlive a deploy should be given a `MaxLifetime`.

## Not established

- **Limits across several instances.** The framework's in-memory rate limiter counts per instance. The test asked for several by allowing one request at a time per instance, but Cloud Run served all twenty concurrent requests from one instance within the ten seconds of the test, so the budget across instances was not measured. A limit that must hold across instances needs a store they share.
- **Cloud Run's second generation execution environment, and regions other than us-central1,** were not compared.
