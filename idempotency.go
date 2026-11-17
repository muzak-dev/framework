package muzak

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default settings for idempotency middleware and [NewMemoryIdempotencyStore].
const (
	DefaultIdempotencyHeader       = "Idempotency-Key"
	DefaultIdempotencyTTL          = 24 * time.Hour
	DefaultIdempotencyLockTTL      = 30 * time.Second
	DefaultIdempotencyMaxBodySize  = 1 << 20
	DefaultIdempotencyStoreTimeout = 2 * time.Second
	DefaultIdempotencyMaxEntries   = 10_000
	idempotencyRecordVersion       = byte(1)
	idempotencyConflictRetryAfter  = "1"
	idempotencyStorageRetryAfter   = "5"
)

// IdempotencyState describes the outcome of [IdempotencyStore.Acquire].
type IdempotencyState uint8

const (
	// IdempotencyNew means no live entry existed and this caller now holds the
	// in-flight lock until [IdempotencyStore.Complete], [IdempotencyStore.Delete],
	// or lockTTL expires.
	IdempotencyNew IdempotencyState = iota
	// IdempotencyInFlight means another request with the same key and digest is
	// currently executing.
	IdempotencyInFlight
	// IdempotencyCompleted means a completed response record was found and
	// returned in record.
	IdempotencyCompleted
	// IdempotencyMismatch means an entry with the same key already exists (in
	// flight or completed) for a different request digest.
	IdempotencyMismatch
)

// IdempotencyStore persists in-flight locks and completed response records for
// idempotent requests; see [IdempotencyOptions].
//
// Keys are opaque SHA-256 digests scoped by method, route pattern, authenticated
// principal (by default), and the caller's Idempotency-Key. Implementations must
// be safe for concurrent use, honor ctx deadlines, and perform Acquire
// atomically so two concurrent requests with the same key never both observe
// [IdempotencyNew]. If the store also satisfies [Lifecycle], the application
// starts it before serving and stops it after draining.
type IdempotencyStore interface {
	// Acquire attempts to claim key for digest for up to lockTTL.
	//
	// If no live entry exists, Acquire stores an in-flight entry for lockTTL
	// with digest and returns (nil, IdempotencyNew, nil). If a live entry exists
	// with a different digest, Acquire returns (nil, IdempotencyMismatch, nil).
	// If a live in-flight entry exists with the same digest, Acquire returns
	// (nil, IdempotencyInFlight, nil). If a completed entry exists with the same
	// digest, Acquire returns its serialized record and IdempotencyCompleted.
	Acquire(ctx context.Context, key string, digest [32]byte, lockTTL time.Duration) (record []byte, state IdempotencyState, err error)

	// Complete stores a completed serialized response record under key for ttl,
	// replacing the in-flight lock.
	Complete(ctx context.Context, key string, record []byte, ttl time.Duration) error

	// Delete removes any in-flight or completed entry under key. Deleting a key
	// that does not exist is not an error.
	Delete(ctx context.Context, key string) error
}

// IdempotencyRecord holds a serialized response cached by an [IdempotencyStore].
type IdempotencyRecord struct {
	Status int         `json:"status"`
	Header http.Header `json:"header,omitzero"`
	Body   []byte      `json:"body,omitzero"`
	Digest [32]byte    `json:"digest"`
}

// EncodeIdempotencyRecord serializes record with a version byte header so mixed
// deployment versions reject incompatible layouts cleanly.
func EncodeIdempotencyRecord(record IdempotencyRecord) ([]byte, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+len(payload))
	out[0] = idempotencyRecordVersion
	copy(out[1:], payload)
	return out, nil
}

// DecodeIdempotencyRecord decodes a record produced by [EncodeIdempotencyRecord].
func DecodeIdempotencyRecord(data []byte) (IdempotencyRecord, error) {
	if len(data) < 3 || data[0] != idempotencyRecordVersion {
		return IdempotencyRecord{}, errors.New("muzak: invalid or unsupported idempotency record version")
	}
	var rec IdempotencyRecord
	if err := json.Unmarshal(data[1:], &rec); err != nil {
		return IdempotencyRecord{}, err
	}
	return rec, nil
}

// IdempotencyOptions configures idempotent request deduplication for an
// application, a router, or a single route.
type IdempotencyOptions struct {
	// Store persists in-flight locks and cached response records. It must be
	// non-nil when idempotency is enabled; use [NewMemoryIdempotencyStore] for
	// tests and single-process deployments.
	Store IdempotencyStore

	// Header names the request header carrying the idempotency key, defaulting
	// to [DefaultIdempotencyHeader] ("Idempotency-Key").
	Header string

	// TTL is how long completed records remain replayable, defaulting to
	// [DefaultIdempotencyTTL] (24h).
	TTL time.Duration

	// LockTTL is how long an in-flight acquisition holds the key before expiring
	// if the executing replica crashes, defaulting to [DefaultIdempotencyLockTTL]
	// (30s).
	LockTTL time.Duration

	// MaxBodySize bounds the response body size eligible for caching, defaulting
	// to [DefaultIdempotencyMaxBodySize] (1 MiB). Responses exceeding this limit
	// still stream to the caller, delete the in-flight key, and are not cached.
	MaxBodySize int

	// StoreTimeout bounds each call to [IdempotencyStore], defaulting to
	// [DefaultIdempotencyStoreTimeout] (2s). A negative value removes the bound.
	StoreTimeout time.Duration

	// FailOpen serves requests without deduplication when [IdempotencyStore]
	// returns an error or times out. By default (false), store failures return
	// 503 Service Unavailable.
	FailOpen bool

	// RequireKey rejects requests on idempotent routes that omit the
	// Idempotency-Key header with 400 Bad Request. By default (false), requests
	// without the header pass through without deduplication.
	RequireKey bool

	// DisablePrincipalScope disables scoping keys by authenticated principal.
	// By default (false), keys include the verified principal so two tenants
	// cannot collide on or leak responses for the same Idempotency-Key.
	DisablePrincipalScope bool
}

func (o IdempotencyOptions) overlay(over IdempotencyOptions) IdempotencyOptions {
	if over.Store != nil {
		o.Store = over.Store
	}
	if over.Header != "" {
		o.Header = over.Header
	}
	if over.TTL != 0 {
		o.TTL = over.TTL
	}
	if over.LockTTL != 0 {
		o.LockTTL = over.LockTTL
	}
	if over.MaxBodySize != 0 {
		o.MaxBodySize = over.MaxBodySize
	}
	if over.StoreTimeout != 0 {
		o.StoreTimeout = over.StoreTimeout
	}
	if over.FailOpen {
		o.FailOpen = true
	}
	if over.RequireKey {
		o.RequireKey = true
	}
	if over.DisablePrincipalScope {
		o.DisablePrincipalScope = true
	}
	return o
}

// WithIdempotency enables request idempotency for a router or route, layering
// optional overrides onto [AppOptions.Idempotency].
func WithIdempotency(opts ...IdempotencyOptions) SharedOption {
	var merged IdempotencyOptions
	for _, o := range opts {
		merged = merged.overlay(o)
	}
	return sharedOption{
		route: func(c *routeConfig) {
			c.idempotency = &merged
			c.idempotencySet = true
		},
		router: func(c *routerConfig) {
			c.idempotency = &merged
			c.idempotencySet = true
		},
	}
}

// SkipIdempotency exempts a route or router from inherited idempotency.
func SkipIdempotency() SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.skipIdempotency = true },
		router: func(c *routerConfig) { c.skipIdempotency = true },
	}
}

// MemoryIdempotencyStore is a bounded, process-local [IdempotencyStore] for
// tests and single-replica deployments.
type MemoryIdempotencyStore struct {
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
}

type memoryIdempotencyEntry struct {
	key       string
	digest    [32]byte
	completed bool
	record    []byte
	expires   time.Time
}

// NewMemoryIdempotencyStore returns a bounded in-memory [IdempotencyStore].
// If maxEntries is omitted or non-positive, [DefaultIdempotencyMaxEntries] is used.
func NewMemoryIdempotencyStore(maxEntries ...int) *MemoryIdempotencyStore {
	limit := DefaultIdempotencyMaxEntries
	if len(maxEntries) > 0 && maxEntries[0] > 0 {
		limit = maxEntries[0]
	}
	return &MemoryIdempotencyStore{
		max:     limit,
		now:     time.Now,
		entries: make(map[string]*list.Element),
		order:   list.New(),
	}
}

// Acquire implements [IdempotencyStore].
func (s *MemoryIdempotencyStore) Acquire(ctx context.Context, key string, digest [32]byte, lockTTL time.Duration) ([]byte, IdempotencyState, error) {
	if err := ctx.Err(); err != nil {
		return nil, IdempotencyNew, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)

	if el, ok := s.entries[key]; ok {
		entry := el.Value.(*memoryIdempotencyEntry)
		if !now.Before(entry.expires) {
			s.removeLocked(el)
		} else {
			s.order.MoveToFront(el)
			if entry.digest != digest {
				return nil, IdempotencyMismatch, nil
			}
			if !entry.completed {
				return nil, IdempotencyInFlight, nil
			}
			return bytes.Clone(entry.record), IdempotencyCompleted, nil
		}
	}

	for len(s.entries) >= s.max {
		back := s.order.Back()
		if back == nil {
			break
		}
		s.removeLocked(back)
	}
	entry := &memoryIdempotencyEntry{
		key:     key,
		digest:  digest,
		expires: now.Add(lockTTL),
	}
	s.entries[key] = s.order.PushFront(entry)
	return nil, IdempotencyNew, nil
}

// Complete implements [IdempotencyStore].
func (s *MemoryIdempotencyStore) Complete(ctx context.Context, key string, record []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)

	el, ok := s.entries[key]
	if !ok {
		for len(s.entries) >= s.max {
			back := s.order.Back()
			if back == nil {
				break
			}
			s.removeLocked(back)
		}
		var digest [32]byte
		if rec, err := DecodeIdempotencyRecord(record); err == nil {
			digest = rec.Digest
		}
		entry := &memoryIdempotencyEntry{
			key:       key,
			digest:    digest,
			completed: true,
			record:    bytes.Clone(record),
			expires:   now.Add(ttl),
		}
		s.entries[key] = s.order.PushFront(entry)
		return nil
	}
	entry := el.Value.(*memoryIdempotencyEntry)
	entry.completed = true
	entry.record = bytes.Clone(record)
	entry.expires = now.Add(ttl)
	s.order.MoveToFront(el)
	return nil
}

// Delete implements [IdempotencyStore].
func (s *MemoryIdempotencyStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.entries[key]; ok {
		s.removeLocked(el)
	}
	return nil
}

func (s *MemoryIdempotencyStore) sweepLocked(now time.Time) {
	for range memorySweep {
		back := s.order.Back()
		if back == nil {
			return
		}
		entry := back.Value.(*memoryIdempotencyEntry)
		if now.Before(entry.expires) {
			return
		}
		s.removeLocked(back)
	}
}

func (s *MemoryIdempotencyStore) removeLocked(el *list.Element) {
	delete(s.entries, el.Value.(*memoryIdempotencyEntry).key)
	s.order.Remove(el)
}

type idempotencyConfig struct {
	store                 IdempotencyStore
	header                string
	ttl                   time.Duration
	lockTTL               time.Duration
	maxBodySize           int
	storeTimeout          time.Duration
	failOpen              bool
	requireKey            bool
	disablePrincipalScope bool
}

func (rt *Route) resolveIdempotency(in inherited) error {
	if in.skipIdempotency || rt.cfg.skipIdempotency {
		rt.skipIdempotency = true
		return nil
	}
	enabled := in.idempotencySet || rt.cfg.idempotencySet
	if !enabled && in.idempotency.Store != nil && isMutatingMethod(rt.Method) {
		enabled = true
	}
	if !enabled {
		return nil
	}
	if rt.sse != nil || rt.websocket != nil {
		return fmt.Errorf("muzak: %s %s: idempotency cannot be enabled on SSE or WebSocket routes", rt.Method, rt.Path)
	}
	opts := in.idempotency
	if rt.cfg.idempotency != nil {
		opts = opts.overlay(*rt.cfg.idempotency)
	}
	if opts.Store == nil {
		return fmt.Errorf("muzak: %s %s: idempotency is enabled without IdempotencyOptions.Store; set a store or pass muzak.NewMemoryIdempotencyStore()", rt.Method, rt.Path)
	}
	cfg := &idempotencyConfig{
		store:                 opts.Store,
		header:                opts.Header,
		ttl:                   opts.TTL,
		lockTTL:               opts.LockTTL,
		maxBodySize:           opts.MaxBodySize,
		storeTimeout:          opts.StoreTimeout,
		failOpen:              opts.FailOpen,
		requireKey:            opts.RequireKey,
		disablePrincipalScope: opts.DisablePrincipalScope,
	}
	if cfg.header == "" {
		cfg.header = DefaultIdempotencyHeader
	}
	cfg.header = textproto.CanonicalMIMEHeaderKey(cfg.header)
	if cfg.ttl <= 0 {
		cfg.ttl = DefaultIdempotencyTTL
	}
	if cfg.lockTTL <= 0 {
		cfg.lockTTL = DefaultIdempotencyLockTTL
	}
	if cfg.maxBodySize <= 0 {
		cfg.maxBodySize = DefaultIdempotencyMaxBodySize
	}
	switch {
	case cfg.storeTimeout == 0:
		cfg.storeTimeout = DefaultIdempotencyStoreTimeout
	case cfg.storeTimeout < 0:
		cfg.storeTimeout = 0
	}
	rt.idempotency = cfg
	return nil
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func (a *App) resolveIdempotency(state *buildState) {
	var stores []IdempotencyStore
	for _, rt := range a.routes {
		if rt.idempotency == nil {
			continue
		}
		if !containsIdempotencyStore(stores, rt.idempotency.store) {
			stores = append(stores, rt.idempotency.store)
		}
	}
	for _, s := range stores {
		if managed, ok := s.(Lifecycle); ok {
			state.lifecycles = append(state.lifecycles, managed)
		}
	}
}

func containsIdempotencyStore(stores []IdempotencyStore, candidate IdempotencyStore) bool {
	for _, existing := range stores {
		if existing == candidate {
			return true
		}
	}
	return false
}

func (cfg *idempotencyConfig) storeCtx(base context.Context, detach bool) (context.Context, context.CancelFunc) {
	if detach {
		base = context.WithoutCancel(base)
	}
	if cfg.storeTimeout <= 0 {
		return base, func() {}
	}
	return context.WithTimeout(base, cfg.storeTimeout)
}

func (cfg *idempotencyConfig) deriveKeyAndDigest(c *Context, rt *Route, rawKey string) (string, [32]byte) {
	var principal [32]byte
	if !cfg.disablePrincipalScope {
		principal = mcpOwner(c)
	}
	toolName := ""
	if a := c.app; a != nil && a.mcp != nil && rt.Path == a.mcp.path && rt.Method == http.MethodPost {
		toolName = extractMCPToolName(c.rawBody)
	}

	keyHasher := sha256.New()
	writeLengthPrefixed(keyHasher, rt.Method)
	writeLengthPrefixed(keyHasher, rt.Path)
	_, _ = keyHasher.Write(principal[:])
	writeLengthPrefixed(keyHasher, toolName)
	writeLengthPrefixed(keyHasher, rawKey)
	keySum := keyHasher.Sum(nil)

	digHasher := sha256.New()
	writeLengthPrefixed(digHasher, rt.Method)
	writeLengthPrefixed(digHasher, rt.Path)
	_, _ = digHasher.Write(c.rawBody)
	var digest [32]byte
	copy(digest[:], digHasher.Sum(nil))

	return fmt.Sprintf("%x", keySum), digest
}

func writeLengthPrefixed(h interface{ Write([]byte) (int, error) }, s string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(s)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(s))
}

func extractMCPToolName(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var env struct {
		Method string         `json:"method"`
		Params jsontext.Value `json:"params"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Method != "tools/call" || len(env.Params) == 0 {
		return ""
	}
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return ""
	}
	return p.Name
}

// beginIdempotency acquires an idempotency slot or replays a completed response.
// When handled is true, the caller must return immediately without running
// dependencies or the route handler.
func (a *App) beginIdempotency(c *Context, route *Route) (finish func(failed bool, panicked bool), handled bool, err error) {
	cfg := route.idempotency
	rawKey, err := singleHeader(c.r.Header, cfg.header)
	if err != nil {
		return nil, false, err
	}
	if rawKey == "" {
		if cfg.requireKey {
			return nil, false, NewHTTPErrorf(http.StatusBadRequest, "the %s header is required", cfg.header)
		}
		return nil, false, nil
	}

	storeKey, digest := cfg.deriveKeyAndDigest(c, route, rawKey)
	ctx, cancel := cfg.storeCtx(c.Context(), false)
	recordBytes, state, storeErr := cfg.store.Acquire(ctx, storeKey, digest, cfg.lockTTL)
	cancel()
	if storeErr != nil {
		a.logger.Log(c.Context(), failureLevel(c.Context(), storeErr, slog.LevelError),
			"muzak: idempotency store Acquire failed",
			slog.String(RequestIDKey, c.RequestID()),
			slog.String("error", storeErr.Error()))
		if cfg.failOpen {
			return nil, false, nil
		}
		c.w.Header().Set(canonicalRetryAfter, idempotencyStorageRetryAfter)
		return nil, false, NewHTTPError(http.StatusServiceUnavailable, "idempotency store is unavailable")
	}

	switch state {
	case IdempotencyMismatch:
		return nil, false, NewHTTPError(http.StatusUnprocessableEntity, "the Idempotency-Key was already used for a different request")
	case IdempotencyInFlight:
		c.w.Header().Set(canonicalRetryAfter, idempotencyConflictRetryAfter)
		return nil, false, NewHTTPError(http.StatusConflict, "a request with this Idempotency-Key is currently in progress")
	case IdempotencyCompleted:
		rec, decErr := DecodeIdempotencyRecord(recordBytes)
		if decErr != nil {
			a.logger.WarnContext(c.Context(), "muzak: corrupt idempotency record deleted",
				slog.String(RequestIDKey, c.RequestID()),
				slog.String("error", decErr.Error()))
			delCtx, delCancel := cfg.storeCtx(c.Context(), true)
			_ = cfg.store.Delete(delCtx, storeKey)
			delCancel()
			if cfg.failOpen {
				return nil, false, nil
			}
			c.w.Header().Set(canonicalRetryAfter, idempotencyStorageRetryAfter)
			return nil, false, NewHTTPError(http.StatusServiceUnavailable, "idempotency store returned a corrupt record")
		}
		markIdempotencyReplayed(c)
		replayIdempotencyRecord(c.w, rec)
		return nil, true, nil
	case IdempotencyNew:
		orig := c.w.ResponseWriter
		capRW := &idempotencyCaptureWriter{
			ResponseWriter: orig,
			maxBody:        cfg.maxBodySize,
		}
		c.w.ResponseWriter = capRW
		finished := false
		finishFn := func(failed bool, panicked bool) {
			if finished {
				return
			}
			finished = true
			c.w.ResponseWriter = orig
			status := c.w.Status()
			if status == 0 {
				status = capRW.status
			}
			if panicked || failed || status == 0 || status >= 500 || capRW.overflowed {
				delCtx, delCancel := cfg.storeCtx(c.Context(), true)
				_ = cfg.store.Delete(delCtx, storeKey)
				delCancel()
				return
			}
			rec := IdempotencyRecord{
				Status: status,
				Header: filterIdempotencyHeaders(capRW.header),
				Body:   capRW.body.Bytes(),
				Digest: digest,
			}
			encoded, encErr := EncodeIdempotencyRecord(rec)
			if encErr != nil {
				delCtx, delCancel := cfg.storeCtx(c.Context(), true)
				_ = cfg.store.Delete(delCtx, storeKey)
				delCancel()
				return
			}
			compCtx, compCancel := cfg.storeCtx(c.Context(), true)
			if compErr := cfg.store.Complete(compCtx, storeKey, encoded, cfg.ttl); compErr != nil {
				a.logger.WarnContext(c.Context(), "muzak: idempotency store Complete failed",
					slog.String(RequestIDKey, c.RequestID()),
					slog.String("error", compErr.Error()))
			}
			compCancel()
		}
		return finishFn, false, nil
	default:
		return nil, false, NewHTTPError(http.StatusServiceUnavailable, "idempotency store returned an unknown state")
	}
}

func markIdempotencyReplayed(c *Context) {
	SpanFromContext(c.Context()).SetAttributes(slog.Bool("idempotency.replayed", true))
	if extra, ok := c.r.Context().Value(accessAttrsKey{}).(*accessLogAttrs); ok {
		extra.idempotencyReplayed = true
	}
}

func replayIdempotencyRecord(w *responseWriter, rec IdempotencyRecord) {
	dst := w.Header()
	for k, vals := range rec.Header {
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
	if len(rec.Body) > 0 {
		dst.Set("Content-Length", strconv.Itoa(len(rec.Body)))
	}
	status := rec.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(rec.Body) > 0 {
		_, _ = w.Write(rec.Body)
	}
}

func filterIdempotencyHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	out := make(http.Header, len(src))
	for k, vals := range src {
		if isExcludedIdempotencyHeader(k) {
			continue
		}
		out[k] = append([]string(nil), vals...)
	}
	return out
}

func isExcludedIdempotencyHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "set-cookie", "content-length":
		return true
	default:
		return false
	}
}

type idempotencyCaptureWriter struct {
	http.ResponseWriter
	maxBody    int
	status     int
	header     http.Header
	body       bytes.Buffer
	overflowed bool
	wroteHead  bool
}

func (w *idempotencyCaptureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *idempotencyCaptureWriter) WriteHeader(status int) {
	if !w.wroteHead {
		w.wroteHead = true
		w.status = status
		w.header = w.ResponseWriter.Header().Clone()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *idempotencyCaptureWriter) Write(p []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	if !w.overflowed {
		if w.body.Len()+len(p) > w.maxBody {
			w.overflowed = true
			w.body.Reset()
		} else {
			_, _ = w.body.Write(p)
		}
	}
	return w.ResponseWriter.Write(p)
}
