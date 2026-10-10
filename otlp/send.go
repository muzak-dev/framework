package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes bounds how much of a collector's answer is read. A
// collector answers an export with a small JSON document at most, and one
// that sends more is not read further: the connection is closed instead.
const maxResponseBytes = 64 << 10

// maxLoggedResponse bounds how much of a refusal's body is quoted in the log.
const maxLoggedResponse = 256

// attempt is the outcome of sending a batch once.
type attempt struct {
	// accepted reports a 2xx, and rejected how many spans the collector
	// nonetheless refused one by one, with its message.
	accepted bool
	rejected int64
	// retry reports an outcome worth sending again: 429, 502, 503, 504, or
	// a failure to deliver at all. retryAfter is the wait the collector asked
	// for, and hasRetryAfter whether it asked.
	retry         bool
	retryAfter    time.Duration
	hasRetryAfter bool
	// status and message describe the outcome for the log.
	status  int
	message string
}

// export sends one batch, retrying it as [Options] describes, and counts the
// outcome.
func (e *Exporter) export(r *run, batch []*span) {
	n := uint64(len(batch))
	if r.ctx.Err() != nil {
		// Stop's deadline has passed: nothing more is sent.
		e.dropped.Add(n)
		r.lost += len(batch)
		return
	}
	body, err := e.encodeBody(batch)
	if err != nil {
		// coverage: every value was converted to a kind the encoder writes
		// when it was recorded, and invalid UTF-8 is replaced rather than
		// refused, so encoding cannot fail; the branch keeps a future kind
		// from being sent half written.
		e.failed.Add(n)
		e.cfg.logger.Error("otlp: a batch of spans could not be encoded", slog.String("error", err.Error()))
		return
	}
	began := time.Now()
	for tries := 0; ; tries++ {
		result := e.send(r.ctx, body)
		if result.accepted {
			// A collector claiming to have rejected more spans than it was
			// sent is held to the batch.
			var rejected uint64
			if result.rejected > 0 {
				rejected = min(uint64(result.rejected), n)
			}
			e.exported.Add(n - rejected)
			if rejected > 0 {
				e.failed.Add(rejected)
				e.cfg.logger.Warn("otlp: the collector rejected some spans",
					slog.Uint64("rejected", rejected), slog.String("message", result.message))
			}
			return
		}
		if !result.retry {
			e.giveUp(r, batch, result, "the collector refused a batch of spans")
			return
		}
		wait := e.retryWait(tries, result)
		if time.Since(began)+wait > e.cfg.retryElapsed {
			e.giveUp(r, batch, result, "a batch of spans could not be delivered in the time allowed for retrying")
			return
		}
		if !e.sleep(r, wait) {
			e.giveUp(r, batch, result, "the exporter stopped before a batch of spans could be delivered")
			return
		}
	}
}

// giveUp counts a batch that was not delivered and logs why. A batch abandoned
// because Stop's deadline passed counts towards what Stop reports as lost.
func (e *Exporter) giveUp(r *run, batch []*span, result attempt, why string) {
	e.failed.Add(uint64(len(batch)))
	if r.ctx.Err() != nil {
		r.lost += len(batch)
	}
	attrs := []any{slog.Int("spans", len(batch))}
	if result.status != 0 {
		attrs = append(attrs, slog.Int("status", result.status))
	}
	if result.message != "" {
		attrs = append(attrs, slog.String("error", result.message))
	}
	e.cfg.logger.Error("otlp: "+why, attrs...)
}

// retryWait returns how long to wait before sending again: an exponential
// backoff with jitter, or what the collector asked for when that is longer,
// capped at RetryMaxInterval.
//
// The backoff starts at RetryInitialInterval and doubles per try up to
// RetryMaxInterval. Half of it is fixed and half drawn at random, which keeps
// the wait from shrinking to nothing while spreading out instances that were
// refused together. A Retry-After can lengthen it but never shorten it: one of
// zero, or a date gone by, answered to every attempt would otherwise have the
// batch sent again as fast as the refusal came back, for all of
// RetryMaxElapsedTime, to a collector that had just said it was overloaded.
func (e *Exporter) retryWait(tries int, result attempt) time.Duration {
	backoff := e.backoff(tries)
	if result.hasRetryAfter {
		return max(backoff, min(result.retryAfter, e.cfg.retryMax))
	}
	return backoff
}

// backoff returns the jittered exponential wait before the retry after tries
// failed ones; see [Exporter.retryWait].
func (e *Exporter) backoff(tries int) time.Duration {
	wait := e.cfg.retryInitial
	for range tries {
		if wait >= e.cfg.retryMax/2 {
			wait = e.cfg.retryMax
			break
		}
		wait *= 2
	}
	wait = min(wait, e.cfg.retryMax)
	half := wait / 2
	// Jitter is not a secret, so the fast generator is the right one.
	return half + rand.N(wait-half+1) //nolint:gosec // jitter needs spread, not unpredictability
}

// sleep waits before a retry, and reports false when the retry should not be
// made: the run was cancelled, or Stop has begun and the wait would end past
// its deadline. A Stop that begins during the wait is checked the same way.
func (e *Exporter) sleep(r *run, wait time.Duration) bool {
	// The wait ends when it is due, which a Stop that begins part way through
	// it compares with its deadline: a whole wait counted from the Stop gave
	// up on retries the deadline could still reach.
	due := time.Now().Add(wait)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	stopping := r.stopping
	for {
		select {
		case <-stopping:
			if !r.stopDeadline.IsZero() && due.After(r.stopDeadline) {
				return false
			}
			// Closed for good, so it is not waited on again.
			stopping = nil
		case <-timer.C:
			return true
		case <-r.ctx.Done():
			return false
		}
	}
}

// send makes one attempt to deliver an encoded batch.
func (e *Exporter) send(ctx context.Context, body []byte) attempt {
	ctx, cancel := context.WithTimeout(ctx, e.cfg.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.url, bytes.NewReader(body))
	if err != nil {
		// coverage: the URL was parsed when the exporter was built and the
		// method is a constant, so building the request cannot fail.
		return attempt{message: err.Error()}
	}
	req.Header = e.cfg.header.Clone()
	resp, err := e.client.Do(req)
	if err != nil {
		// Nothing was delivered: a refused connection, a timeout, a reset.
		// Worth another try, unless it was the run being cancelled.
		return attempt{retry: ctx.Err() == nil || errors.Is(context.Cause(ctx), context.DeadlineExceeded), message: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	complete := readErr == nil && len(data) <= maxResponseBytes
	switch status := resp.StatusCode; {
	case status >= 200 && status < 300:
		result := attempt{accepted: true, status: status}
		if complete {
			result.rejected, result.message = parsePartialSuccess(data)
		}
		return result
	case status == http.StatusTooManyRequests || status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
		result := attempt{retry: true, status: status, message: summarize(data)}
		result.retryAfter, result.hasRetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return result
	default:
		return attempt{status: status, message: summarize(data)}
	}
}

// summarize returns the start of a refusal's body for the log, cut to
// [maxLoggedResponse]. The logger escapes whatever it holds.
func summarize(data []byte) string {
	return truncate(strings.TrimSpace(string(data[:min(len(data), maxLoggedResponse+utf8MaxTail)])), maxLoggedResponse)
}

// utf8MaxTail is how many bytes past a cut [truncate] may need to look at to
// find the start of a character.
const utf8MaxTail = 4

// parseRetryAfter reads a Retry-After header, which is either a number of
// seconds or an HTTP date, and reports false for one that is neither. A date
// in the past is a wait of zero. Seconds past what a Duration holds are read
// as the longest one, which the caller caps.
func parseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if header[0] >= '0' && header[0] <= '9' {
		seconds, err := strconv.ParseUint(header, 10, 64)
		if err != nil {
			var numErr *strconv.NumError
			if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) {
				return time.Duration(1<<63 - 1), true
			}
			return 0, false
		}
		if seconds > uint64((1<<63-1)/time.Second) {
			return time.Duration(1<<63 - 1), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(header)
	if err != nil {
		return 0, false
	}
	return max(at.Sub(now), 0), true
}

// partialSuccess is the part of an export's answer that reports spans the
// collector accepted the request for but did not keep.
type partialSuccess struct {
	PartialSuccess struct {
		RejectedSpans jsontext.Value `json:"rejectedSpans"`
		ErrorMessage  string         `json:"errorMessage"`
	} `json:"partialSuccess"`
}

// parsePartialSuccess reads how many spans a collector rejected from the body
// of a successful answer, and its message, cut to [maxLoggedResponse]. The
// count is an int64, which the JSON encoding writes as a string and which a
// number is accepted for too; anything that is not a non-negative count reads
// as none. A body that is not the expected JSON, which a collector may send
// for a success, reads as no rejection.
func parsePartialSuccess(data []byte) (int64, string) {
	var answer partialSuccess
	if len(data) == 0 || json.Unmarshal(data, &answer) != nil {
		return 0, ""
	}
	raw := strings.Trim(string(answer.PartialSuccess.RejectedSpans), `"`)
	rejected, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || rejected < 0 {
		rejected = 0
	}
	return rejected, truncate(answer.PartialSuccess.ErrorMessage, maxLoggedResponse)
}

// encodeBody encodes a batch as the body of an export request, compressed
// when the exporter is configured to.
func (e *Exporter) encodeBody(batch []*span) ([]byte, error) {
	var buf bytes.Buffer
	if !e.cfg.gzip {
		err := e.encode(&buf, batch)
		return buf.Bytes(), err
	}
	zw := gzip.NewWriter(&buf)
	err := errors.Join(e.encode(zw, batch), zw.Close())
	return buf.Bytes(), err
}
