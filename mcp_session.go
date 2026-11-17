package muzak

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"sync"
	"time"
)

const mcpSessionRecordVersion = byte(1)

// MCPSessionRecord holds the negotiated revision and owner digest of a legacy
// MCP session stored in an [MCPSessionStore].
type MCPSessionRecord struct {
	Version string            `json:"version"`
	Owner   [sha256.Size]byte `json:"owner"`
}

func (r MCPSessionRecord) ownedBy(owner [sha256.Size]byte) bool {
	return r.Owner == [sha256.Size]byte{} || r.Owner == owner
}

// EncodeMCPSessionRecord serializes record with a version byte header so mixed
// deployment versions reject incompatible layouts cleanly.
func EncodeMCPSessionRecord(record MCPSessionRecord) ([]byte, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+len(payload))
	out[0] = mcpSessionRecordVersion
	copy(out[1:], payload)
	return out, nil
}

// DecodeMCPSessionRecord decodes a record produced by [EncodeMCPSessionRecord].
func DecodeMCPSessionRecord(data []byte) (MCPSessionRecord, error) {
	if len(data) < 3 || data[0] != mcpSessionRecordVersion {
		return MCPSessionRecord{}, errors.New("muzak: invalid or unsupported MCP session record version")
	}
	var rec MCPSessionRecord
	if err := json.Unmarshal(data[1:], &rec); err != nil {
		return MCPSessionRecord{}, err
	}
	return rec, nil
}

// MCPSessionStore persists legacy MCP client sessions across requests and
// replicas; see [MCPOptions.SessionStore].
//
// Implementations must be safe for concurrent use and honor ctx deadlines.
// Lookup must atomically refresh the session's idle TTL when found. If the
// store also satisfies [Lifecycle], the application starts it before serving
// and stops it after draining.
type MCPSessionStore interface {
	Create(ctx context.Context, version string, owner [sha256.Size]byte, ttl time.Duration) (id string, err error)
	Lookup(ctx context.Context, id string, ttl time.Duration) (record MCPSessionRecord, found bool, err error)
	Delete(ctx context.Context, id string) error
}

// mcpSessions holds the sessions legacy clients open with initialize.
//
// A session holds no authority. Every request is authenticated on its own by
// the endpoint's security, and a tool call by its route's, so what a session
// keeps is the revision its client negotiated and who opened it. Identifiers
// are 128 random bits from crypto/rand, written as 26 characters of base32,
// which the specification's visible ASCII admits. Only their SHA-256 is held,
// so a heap dump or a log of the map yields nothing a client could present,
// and a lookup's timing depends on a digest the caller cannot steer.
//
// The store is bounded: at most limit sessions, each dropped once it has been
// idle for idle. Sessions are kept in the order they were last used, so the
// expired ones are at the back and every operation is constant time, amortized
// over the expired sessions it drops. A session that would go past the limit
// evicts the one idle longest, whose client is answered 404 and opens another,
// as the protocol says it must.
type mcpSessions struct {
	mu    sync.Mutex
	index map[[sha256.Size]byte]*list.Element
	order list.List
	limit int
	idle  time.Duration
	// now is the clock, replaced by tests of expiry.
	now func() time.Time
}

// mcpSession is one open session.
type mcpSession struct {
	key [sha256.Size]byte
	// version is the revision initialize negotiated.
	version string
	// owner identifies the principal that opened the session, and is zero
	// when the endpoint verified none; see [mcpOwner].
	owner    [sha256.Size]byte
	lastSeen time.Time
}

// mcpSessionIDLength is the length of an identifier rand.Text writes.
const mcpSessionIDLength = 26

// newMCPSessions returns an empty store.
func newMCPSessions(limit int, idle time.Duration) *mcpSessions {
	return &mcpSessions{
		index: make(map[[sha256.Size]byte]*list.Element),
		limit: limit,
		idle:  idle,
		now:   time.Now,
	}
}

// Create implements [MCPSessionStore].
func (s *mcpSessions) Create(ctx context.Context, version string, owner [sha256.Size]byte, _ time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.create(version, owner), nil
}

// Lookup implements [MCPSessionStore].
func (s *mcpSessions) Lookup(ctx context.Context, id string, _ time.Duration) (MCPSessionRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return MCPSessionRecord{}, false, err
	}
	session, ok := s.lookup(id)
	if !ok {
		return MCPSessionRecord{}, false, nil
	}
	return MCPSessionRecord{Version: session.version, Owner: session.owner}, true, nil
}

// Delete implements [MCPSessionStore].
func (s *mcpSessions) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.remove(id)
	return nil
}

// create opens a session and returns its identifier.
func (s *mcpSessions) create(version string, owner [sha256.Size]byte) string {
	id := rand.Text()
	session := &mcpSession{key: sha256.Sum256([]byte(id)), version: version, owner: owner}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expireLocked(now)
	for s.order.Len() >= s.limit {
		s.removeLocked(s.order.Back())
	}
	session.lastSeen = now
	s.index[session.key] = s.order.PushFront(session)
	return id
}

// lookup returns the session an identifier names and marks it used, or
// reports that there is none, because it never existed, expired or ended.
func (s *mcpSessions) lookup(id string) (mcpSession, bool) {
	if !wellFormedSessionID(id) {
		return mcpSession{}, false
	}
	key := sha256.Sum256([]byte(id))
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expireLocked(now)
	element, ok := s.index[key]
	if !ok {
		return mcpSession{}, false
	}
	session := element.Value.(*mcpSession)
	session.lastSeen = now
	s.order.MoveToFront(element)
	return *session, true
}

// remove ends a session, reporting whether there was one.
func (s *mcpSessions) remove(id string) bool {
	if !wellFormedSessionID(id) {
		return false
	}
	key := sha256.Sum256([]byte(id))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(s.now())
	element, ok := s.index[key]
	if ok {
		s.removeLocked(element)
	}
	return ok
}

// len reports how many sessions are held.
func (s *mcpSessions) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.order.Len()
}

// expireLocked drops every session idle for longer than the timeout. They are
// all at the back, since sessions are ordered by when they were last used.
func (s *mcpSessions) expireLocked(now time.Time) {
	for back := s.order.Back(); back != nil; back = s.order.Back() {
		if now.Sub(back.Value.(*mcpSession).lastSeen) <= s.idle {
			return
		}
		s.removeLocked(back)
	}
}

// removeLocked drops one session.
func (s *mcpSessions) removeLocked(element *list.Element) {
	delete(s.index, element.Value.(*mcpSession).key)
	s.order.Remove(element)
}

// wellFormedSessionID reports whether a header value could be an identifier
// this store issued, which keeps a value of any other shape from being
// hashed: rand.Text writes 26 characters of the base32 alphabet.
func wellFormedSessionID(id string) bool {
	if len(id) != mcpSessionIDLength {
		return false
	}
	for i := range len(id) {
		if c := id[i]; (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}
