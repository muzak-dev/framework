package muzak

import (
	"context"
	crand "crypto/rand"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// retryPolicy decides whether a request is sent again, and after how long.
type retryPolicy struct {
	maxAttempts   int
	baseDelay     time.Duration
	maxDelay      time.Duration
	maxRetryAfter time.Duration
	budget        *retryBudget

	// now, wait and jitter are what a test replaces to run without waiting:
	// the clock a Retry-After date is read against, the pause between
	// attempts, and the random part of it.
	now    func() time.Time
	wait   func(ctx context.Context, d time.Duration) error
	jitter func(ceiling time.Duration) time.Duration
}

// newRetryPolicy resolves the retry settings of a set of options.
func newRetryPolicy(opts ClientOptions) *retryPolicy {
	policy := &retryPolicy{
		maxAttempts:   opts.MaxAttempts,
		baseDelay:     opts.RetryBaseDelay,
		maxDelay:      opts.RetryMaxDelay,
		maxRetryAfter: opts.MaxRetryAfter,
		budget:        newRetryBudget(opts.RetryBudget),
		now:           time.Now,
		wait:          sleepContext,
		jitter:        fullJitter,
	}
	if policy.maxAttempts <= 0 {
		policy.maxAttempts = DefaultClientMaxAttempts
	}
	if policy.baseDelay <= 0 {
		policy.baseDelay = DefaultClientRetryBaseDelay
	}
	if policy.maxDelay <= 0 {
		policy.maxDelay = DefaultClientRetryMaxDelay
	}
	policy.maxDelay = max(policy.maxDelay, policy.baseDelay)
	if policy.maxRetryAfter <= 0 {
		policy.maxRetryAfter = DefaultClientMaxRetryAfter
	}
	return policy
}

// next reports whether a failed attempt is followed by another, and how long
// to wait first.
//
// It says no when the request cannot be replayed, when the attempts are used
// up, when the server asked for a longer wait than MaxRetryAfter allows, when
// the wait would outlast the call's deadline, which would only spend the
// deadline sleeping, and when the retry budget is empty. The budget is asked
// last, so that a retry that was never going to be made does not spend it.
func (p *retryPolicy) next(ctx context.Context, attempt int, replayable bool, resp *http.Response) (time.Duration, bool) {
	if !replayable || attempt >= p.maxAttempts {
		return 0, false
	}
	delay := p.backoff(attempt)
	if resp != nil {
		if after, ok := parseRetryAfter(resp.Header.Get("Retry-After"), p.now()); ok {
			if after > p.maxRetryAfter {
				return 0, false
			}
			// The server's wait is a floor rather than a replacement: clients
			// told the same Retry-After would otherwise all come back in the
			// same instant.
			delay = max(delay, after)
		}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
		return 0, false
	}
	return delay, p.budget.spend()
}

// backoff returns the wait before the attempt after this one: a random
// duration up to a ceiling that doubles from the base delay with each attempt
// and stops at the maximum. Drawing the whole wait at random rather than
// adding a little noise to a fixed one is what spreads out clients that all
// failed at the same moment.
func (p *retryPolicy) backoff(attempt int) time.Duration {
	ceiling := p.baseDelay
	for range attempt - 1 {
		if ceiling >= p.maxDelay/2 {
			ceiling = p.maxDelay
			break
		}
		ceiling *= 2
	}
	return p.jitter(min(ceiling, p.maxDelay))
}

// fullJitter returns a duration chosen uniformly between zero and ceiling.
//
// The draw comes from crypto/rand. The wait needs no secrecy, but the
// generator costs nothing that matters at one draw per retry, and it keeps the
// module free of the weak one a security linter rightly flags everywhere else.
func fullJitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	// The bound is ceiling+1, so that ceiling itself can be drawn, worked out
	// in a big.Int because it does not fit an int64 when ceiling is the
	// largest duration there is. The draw is below it, so it always does.
	bound := new(big.Int).Add(big.NewInt(int64(ceiling)), big.NewInt(1))
	n, _ := crand.Int(crand.Reader, bound)
	return time.Duration(n.Int64())
}

// sleepContext waits for a duration or until the context ends, reporting the
// context's cause in the second case.
func sleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// maxRetryAfterDate bounds the length of a Retry-After header read as a date.
// The three date formats HTTP allows are under thirty bytes, and anything
// much longer is not one of them.
const maxRetryAfterDate = 64

// parseRetryAfter reads a Retry-After header, which RFC 9110 allows to be a
// number of seconds or an HTTP date, and returns how long from now it asks the
// client to wait.
//
// A date in the past is a wait of zero, which is what it means. A number too
// large for a duration saturates rather than wrapping into a negative wait,
// and is then refused by the caller's MaxRetryAfter like any other long wait.
// Anything else, a sign, a fraction or a date in no format HTTP allows, is
// not a Retry-After at all and is ignored.
//
// The cost is linear in the length of the value, and a date is not parsed past
// maxRetryAfterDate bytes.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.Trim(value, " \t")
	if value == "" {
		return 0, false
	}
	if strings.Trim(value, "0123456789") == "" {
		const most = uint64(math.MaxInt64 / int64(time.Second))
		var seconds uint64
		for i := range len(value) {
			if seconds > most {
				// Every digit that follows only makes it larger, so there is
				// no need to keep multiplying towards an overflow.
				break
			}
			seconds = seconds*10 + uint64(value[i]-'0')
		}
		if seconds > most {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if len(value) > maxRetryAfterDate {
		return 0, false
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	// Sub saturates rather than overflowing for a date centuries away.
	return max(when.Sub(now), 0), true
}

// retryTokenUnit is one retry in the budget's arithmetic, which counts in
// thousandths so that a ratio such as 0.1 is exact without floating point on
// every request.
const retryTokenUnit = 1000

// maxRetryBurst bounds the burst a budget is configured with, which keeps its
// arithmetic far from overflowing.
const maxRetryBurst = 1 << 20

// retryBudget is the bucket [RetryBudget] describes. It is shared by every
// call a client makes, and is lock free: a successful response on a full
// bucket, which is the common case, costs one atomic load.
type retryBudget struct {
	tokens atomic.Int64
	earned int64
	most   int64
}

// newRetryBudget builds a full bucket from its options.
func newRetryBudget(opts RetryBudget) *retryBudget {
	ratio := opts.Ratio
	if !(ratio > 0) {
		// NaN fails the comparison too, which is the point of writing it
		// this way round.
		ratio = defaultRetryRatio
	}
	burst := opts.Burst
	if burst <= 0 {
		burst = defaultRetryBurst
	}
	burst = min(burst, maxRetryBurst)
	budget := &retryBudget{
		earned: max(int64(math.Ceil(min(ratio, float64(burst))*retryTokenUnit)), 1),
		most:   int64(burst) * retryTokenUnit,
	}
	budget.tokens.Store(budget.most)
	return budget
}

// earn credits the bucket for a response that succeeded.
func (b *retryBudget) earn() {
	for {
		current := b.tokens.Load()
		if current >= b.most {
			return
		}
		if b.tokens.CompareAndSwap(current, min(b.most, current+b.earned)) {
			return
		}
	}
}

// spend takes one retry from the bucket, reporting false when there is not a
// whole one to take.
func (b *retryBudget) spend() bool {
	for {
		current := b.tokens.Load()
		if current < retryTokenUnit {
			return false
		}
		if b.tokens.CompareAndSwap(current, current-retryTokenUnit) {
			return true
		}
	}
}
