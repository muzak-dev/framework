package muzak

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// waitRecorder replaces a client's pause between attempts with one that
// records how long it was asked to wait and returns at once.
type waitRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func recordWaits(client *Client) *waitRecorder {
	recorder := &waitRecorder{}
	client.retry.wait = func(ctx context.Context, d time.Duration) error {
		recorder.mu.Lock()
		recorder.waits = append(recorder.waits, d)
		recorder.mu.Unlock()
		return ctx.Err()
	}
	return recorder
}

func (r *waitRecorder) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.waits...)
}

// statusServer answers with each status in turn, and with the last one once
// they run out.
func statusServer(t *testing.T, header http.Header, statuses ...int) *countingServer {
	t.Helper()
	var served atomic.Int32
	return newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n := int(served.Add(1)) - 1
		for name, values := range header {
			w.Header()[name] = values
		}
		w.WriteHeader(statuses[min(n, len(statuses)-1)])
		_, _ = w.Write([]byte("attempt " + strconv.Itoa(n+1)))
	})
}

func TestClientRetriesOnlyTheStatusesThatMayClear(t *testing.T) {
	t.Parallel()
	for _, status := range []int{429, 502, 503, 504, 500, 501, 400, 404, 409} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			server := statusServer(t, nil, status)
			client, network := newTestClient(t, ClientOptions{})
			network.serve("api.example.com", publicA, "80", server.Server)
			resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			body := drain(t, resp)
			want := int32(1)
			if retryableStatus(status) {
				want = DefaultClientMaxAttempts
			}
			if hits := server.hits.Load(); hits != want {
				t.Errorf("hits = %d, want %d", hits, want)
			}
			// The last answer is the caller's to read, body and all.
			if resp.StatusCode != status || body != "attempt "+strconv.Itoa(int(want)) {
				t.Errorf("response = %d %q, want the last attempt's", resp.StatusCode, body)
			}
		})
	}
}

func TestClientRetrySucceedsAfterTransientFailures(t *testing.T) {
	t.Parallel()
	server := statusServer(t, nil, 503, 502, 200)
	client, network := newTestClient(t, ClientOptions{RetryBaseDelay: 100 * time.Millisecond})
	client.retry.jitter = func(ceiling time.Duration) time.Duration { return ceiling }
	waits := recordWaits(client)
	network.serve("api.example.com", publicA, "80", server.Server)
	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if body := drain(t, resp); resp.StatusCode != 200 || body != "attempt 3" {
		t.Errorf("response = %d %q, want the third attempt's success", resp.StatusCode, body)
	}
	// The ceiling doubles from the base delay with each attempt.
	if got := waits.recorded(); len(got) != 2 || got[0] != 100*time.Millisecond || got[1] != 200*time.Millisecond {
		t.Errorf("waits = %v, want [100ms 200ms]", got)
	}
}

func TestClientRetriesOnlyWhatIsSafeToSendTwice(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		header string
		want   int32
	}{
		{http.MethodGet, "", 3},
		{http.MethodHead, "", 3},
		{http.MethodOptions, "", 3},
		{http.MethodPut, "", 3},
		{http.MethodDelete, "", 3},
		{http.MethodPost, "", 1},
		{http.MethodPatch, "", 1},
		{"TRACE", "", 1},
		{http.MethodPost, "Idempotency-Key", 3},
		{http.MethodPatch, "X-Idempotency-Key", 3},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.header, func(t *testing.T) {
			t.Parallel()
			server := statusServer(t, nil, 503)
			client, network := newTestClient(t, ClientOptions{})
			network.serve("api.example.com", publicA, "80", server.Server)
			req := mustRequest(t, t.Context(), tc.method, "http://api.example.com/", `{"n":1}`)
			if tc.header != "" {
				req.Header.Set(tc.header, "7c0e5f4a")
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			drain(t, resp)
			if hits := server.hits.Load(); hits != tc.want {
				t.Errorf("%s with %q: hits = %d, want %d", tc.method, tc.header, hits, tc.want)
			}
		})
	}
}

func TestClientReplaysTheBodyOrDoesNotRetry(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var bodies []string
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	client, network := newTestClient(t, ClientOptions{})
	network.serve("api.example.com", publicA, "80", server.Server)

	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodPut, "http://api.example.com/", "payload"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drain(t, resp)
	mu.Lock()
	if len(bodies) != 3 || bodies[0] != "payload" || bodies[1] != "payload" || bodies[2] != "payload" {
		t.Errorf("bodies = %q, want the payload sent whole on every attempt", bodies)
	}
	bodies = nil
	mu.Unlock()

	// A body that can only be read once cannot be sent a second time, so the
	// request is not retried rather than retried with nothing.
	once := mustRequest(t, t.Context(), http.MethodPut, "http://api.example.com/")
	once.Body = io.NopCloser(strings.NewReader("streamed"))
	resp, err = client.Do(once)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drain(t, resp)
	mu.Lock()
	if len(bodies) != 1 || bodies[0] != "streamed" {
		t.Errorf("bodies = %q, want one attempt", bodies)
	}
	mu.Unlock()

	failing := mustRequest(t, t.Context(), http.MethodPut, "http://api.example.com/", "payload")
	failing.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("the source is gone") }
	_, err = client.Do(failing)
	if err == nil || !strings.Contains(err.Error(), "could not be read again") {
		t.Errorf("Do error = %v, want the replay failure reported", err)
	}
}

func TestClientHonoursRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		retryAfter string
		hits       int32
		waits      []time.Duration
	}{
		{"seconds", "2", 3, []time.Duration{2 * time.Second, 2 * time.Second}},
		{"date", now.Add(3 * time.Second).Format(http.TimeFormat), 3, []time.Duration{3 * time.Second, 3 * time.Second}},
		{"past date", now.Add(-time.Hour).Format(http.TimeFormat), 3, []time.Duration{0, 0}},
		{"zero", "0", 3, []time.Duration{0, 0}},
		{"not a wait", "soon", 3, []time.Duration{0, 0}},
		{"longer than allowed", "11", 1, nil},
		{"an hour", "3600", 1, nil},
		{"overflowing", strings.Repeat("9", 40), 1, nil},
		{"date far away", "Fri, 31 Dec 9999 23:59:59 GMT", 1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := statusServer(t, http.Header{"Retry-After": {tc.retryAfter}}, 429)
			client, network := newTestClient(t, ClientOptions{})
			client.retry.now = func() time.Time { return now }
			waits := recordWaits(client)
			network.serve("api.example.com", publicA, "80", server.Server)
			resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			drain(t, resp)
			if hits := server.hits.Load(); hits != tc.hits {
				t.Errorf("hits = %d, want %d", hits, tc.hits)
			}
			got := waits.recorded()
			if len(got) != len(tc.waits) {
				t.Fatalf("waits = %v, want %v", got, tc.waits)
			}
			for i := range got {
				if got[i] != tc.waits[i] {
					t.Errorf("waits = %v, want %v", got, tc.waits)
				}
			}
			if tc.hits == 1 && resp.Header.Get("Retry-After") != tc.retryAfter {
				t.Errorf("Retry-After = %q, want the server's answer handed back intact", resp.Header.Get("Retry-After"))
			}
		})
	}
}

func TestClientDoesNotSleepPastTheDeadline(t *testing.T) {
	t.Parallel()
	server := statusServer(t, http.Header{"Retry-After": {"5"}}, 503)
	client, network := newTestClient(t, ClientOptions{})
	waits := recordWaits(client)
	network.serve("api.example.com", publicA, "80", server.Server)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	resp, err := client.Do(mustRequest(t, ctx, http.MethodGet, "http://api.example.com/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drain(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || len(waits.recorded()) != 0 || server.hits.Load() != 1 {
		t.Errorf("status %d after %d hits and waits %v, want the 503 returned at once rather than a sleep into the deadline",
			resp.StatusCode, server.hits.Load(), waits.recorded())
	}
}

func TestClientRetriesConnectionErrors(t *testing.T) {
	t.Parallel()
	server := statusServer(t, nil, 200)
	client, network := newTestClient(t, ClientOptions{})
	network.serve("api.example.com", publicA, "80", server.Server)
	network.failures.Store(2)
	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drain(t, resp)
	if dialled := len(network.connections()); dialled != 3 || server.hits.Load() != 1 {
		t.Errorf("connections = %d, hits = %d, want two refusals and then a success", dialled, server.hits.Load())
	}

	// The connection the GET left idle would be reused otherwise, and the
	// point is a connection that fails to open.
	_ = client.Close()
	network.failures.Store(5)
	_, err = client.Do(mustRequest(t, t.Context(), http.MethodPost, "http://api.example.com/", "{}"))
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("POST error = %v, want the connection refusal", err)
	}
	if dialled := len(network.connections()); dialled != 4 {
		t.Errorf("connections = %d, want the POST tried once", dialled)
	}

	_, err = client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("GET error = %v, want it to say how many attempts were made", err)
	}
}

func TestClientRetriesAConnectionTheServerDrops(t *testing.T) {
	t.Parallel()
	var dropped atomic.Int32
	server := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if dropped.Add(1) <= 2 {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte("recovered"))
	})
	client, network := newTestClient(t, ClientOptions{})
	network.serve("api.example.com", publicA, "80", server.Server)
	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if body := drain(t, resp); body != "recovered" {
		t.Errorf("body = %q, want the third attempt's", body)
	}
}

func TestClientRetryBudgetStopsARetryStorm(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	server := newCountingServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(status.Load())) })
	client, network := newTestClient(t, ClientOptions{RetryBudget: RetryBudget{Burst: 2, Ratio: 0.5}})
	network.serve("api.example.com", publicA, "80", server.Server)
	send := func() {
		t.Helper()
		resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		drain(t, resp)
	}

	// The burst pays for two retries, and then the upstream sees one request
	// per call rather than three.
	for range 3 {
		send()
	}
	if hits := server.hits.Load(); hits != 5 {
		t.Errorf("hits = %d, want 3 + 1 + 1 once the burst of 2 is spent", hits)
	}

	// Two successes at a ratio of a half earn one retry back.
	status.Store(http.StatusOK)
	send()
	send()
	status.Store(http.StatusServiceUnavailable)
	server.hits.Store(0)
	send()
	if hits := server.hits.Load(); hits != 2 {
		t.Errorf("hits = %d, want one earned retry spent", hits)
	}
}

func TestRetryBudgetUnderConcurrency(t *testing.T) {
	t.Parallel()
	budget := newRetryBudget(RetryBudget{Burst: 5, Ratio: 0.25})
	var spent atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if budget.spend() {
				spent.Add(1)
			}
		})
	}
	wg.Wait()
	if spent.Load() != 5 {
		t.Errorf("spent %d retries from a burst of 5", spent.Load())
	}
	for range 64 {
		wg.Go(budget.earn)
	}
	wg.Wait()
	if tokens := budget.tokens.Load(); tokens != budget.most {
		t.Errorf("tokens = %d after 64 earnings of a quarter, want the cap of %d", tokens, budget.most)
	}

	for _, opts := range []RetryBudget{{Ratio: math.NaN()}, {Ratio: -1}, {Ratio: math.Inf(1)}, {Burst: math.MaxInt}, {Burst: -3}} {
		b := newRetryBudget(opts)
		if b.earned < 1 || b.most < retryTokenUnit || b.most > maxRetryBurst*retryTokenUnit || b.earned > b.most {
			t.Errorf("newRetryBudget(%+v) = earned %d, most %d, want sane bounds", opts, b.earned, b.most)
		}
	}
}

func TestClientCancellationEndsTheWaitBetweenAttempts(t *testing.T) {
	t.Parallel()
	server := statusServer(t, http.Header{"Retry-After": {"5"}}, 503)
	client, network := newTestClient(t, ClientOptions{})
	client.retry.wait = sleepContext
	network.serve("api.example.com", publicA, "80", server.Server)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := client.Do(mustRequest(t, ctx, http.MethodGet, "http://api.example.com/"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Do took %v, want the five second wait cut short", elapsed)
	}
	if hits := server.hits.Load(); hits != 1 {
		t.Errorf("hits = %d, want the second attempt never sent", hits)
	}
}

func TestClientTimeoutSpansEveryAttempt(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	client, network := newTestClient(t, ClientOptions{Timeout: 150 * time.Millisecond})
	network.serve("api.example.com", publicA, "80", server.Server)
	start := time.Now()
	_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/secret?token=TOKEN"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "did not complete within 150ms") || strings.Contains(err.Error(), "TOKEN") {
		t.Errorf("error = %q, want the timeout named and the URL's path and query left out", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Do took %v, want it bounded by the timeout", elapsed)
	}
}

func TestRetryBackoffBounds(t *testing.T) {
	t.Parallel()
	policy := newRetryPolicy(ClientOptions{RetryBaseDelay: time.Second, RetryMaxDelay: 5 * time.Second})
	policy.jitter = func(ceiling time.Duration) time.Duration { return ceiling }
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 5 * time.Second, 60: 5 * time.Second} {
		if got := policy.backoff(attempt); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
	huge := newRetryPolicy(ClientOptions{RetryBaseDelay: time.Hour, RetryMaxDelay: time.Duration(math.MaxInt64)})
	huge.jitter = func(ceiling time.Duration) time.Duration { return ceiling }
	if got := huge.backoff(1000); got <= 0 {
		t.Errorf("backoff(1000) = %v, want no overflow into a negative wait", got)
	}
	// A maximum below the base is raised to it.
	if p := newRetryPolicy(ClientOptions{RetryBaseDelay: time.Second, RetryMaxDelay: time.Millisecond}); p.maxDelay != time.Second {
		t.Errorf("maxDelay = %v, want it raised to the base", p.maxDelay)
	}
	for range 1000 {
		if got := fullJitter(10 * time.Millisecond); got < 0 || got > 10*time.Millisecond {
			t.Fatalf("fullJitter = %v, want it within [0, 10ms]", got)
		}
	}
	if fullJitter(0) != 0 || fullJitter(-time.Second) != 0 {
		t.Error("fullJitter of nothing is not nothing")
	}
	if got := fullJitter(time.Duration(math.MaxInt64)); got < 0 {
		t.Errorf("fullJitter(max) = %v, want it non-negative", got)
	}
}

func TestSleepContext(t *testing.T) {
	t.Parallel()
	if err := sleepContext(t.Context(), time.Millisecond); err != nil {
		t.Errorf("sleepContext = %v, want nil", err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("muzak: stopped")
	cancel(cause)
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, cause) {
		t.Errorf("sleepContext on an ended context = %v, want its cause", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"0", 0, true},
		{"120", 2 * time.Minute, true},
		{" 7 ", 7 * time.Second, true},
		{"\t3", 3 * time.Second, true},
		{"0000005", 5 * time.Second, true},
		{strings.Repeat("9", 100), time.Duration(math.MaxInt64), true},
		{"9223372037", time.Duration(math.MaxInt64), true},
		{"9223372036", 9223372036 * time.Second, true},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{"Friday, 09-Oct-26 12:00:30 GMT", 30 * time.Second, true},
		{"Fri Oct  9 12:01:00 2026", time.Minute, true},
		{now.Add(-24 * time.Hour).Format(http.TimeFormat), 0, true},
		{"Thu, 01 Jan 1970 00:00:00 GMT", 0, true},
		{"", 0, false},
		{"   ", 0, false},
		{"-1", 0, false},
		{"+5", 0, false},
		{"1.5", 0, false},
		{"1e3", 0, false},
		{"5s", 0, false},
		{"tomorrow", 0, false},
		{"Fri, 09 Oct 2026 12:00:30 GMT  ", 30 * time.Second, true},
		{"Fri, 09 Oct 2026 12:00:30 GMT" + strings.Repeat("x", 60), 0, false},
		{"Fri, 09 Oct 2026 12:00:30 GMT; extra", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.value, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) = %v, %v, want %v, %v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRetryableErrorClassification(t *testing.T) {
	t.Parallel()
	timeoutErr := &url.Error{Op: "Get", URL: "http://x", Err: &timeoutError{}}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"refused connection": {&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		"reset":              {&url.Error{Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}, true},
		"eof":                {&url.Error{Err: io.EOF}, true},
		"unexpected eof":     {io.ErrUnexpectedEOF, true},
		"header timeout":     {timeoutErr, true},
		"tls alert":          {&net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}, false},
		"dns not found":      {&net.OpError{Op: "dial", Err: &net.DNSError{IsNotFound: true}}, false},
		"dns temporary":      {&net.DNSError{IsTemporary: true}, true},
		"refused address":    {&url.Error{Err: &AddressRefusedError{}}, false},
		"circuit open":       {&url.Error{Err: &circuitOpenError{}}, false},
		"redirect refused":   {&url.Error{Err: &redirectRefusal{}}, false},
		"something else":     {&url.Error{Err: errors.New("net/http: unsupported protocol")}, false},
	} {
		if got := retryableError(t.Context(), tc.err); got != tc.want {
			t.Errorf("%s: retryableError = %v, want %v", name, got, tc.want)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if retryableError(ctx, io.EOF) {
		t.Error("an error after the call was cancelled was judged retryable")
	}
}

// timeoutError is an error that says it is a timeout, as net/http's
// response header timeout does.
type timeoutError struct{}

func (*timeoutError) Error() string   { return "net/http: timeout awaiting response headers" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }
