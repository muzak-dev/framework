package muzak

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDB is a database/sql driver that keeps nothing but a record of what it
// was asked to do, and fails where a test tells it to.
type fakeDB struct {
	mu        sync.Mutex
	events    []string
	beginErr  error
	commitErr error
}

func (d *fakeDB) record(event string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, event)
}

func (d *fakeDB) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.events, ", ")
}

// waitFor waits until the record reads want, for the rollback database/sql
// performs on a goroutine of its own once a context is cancelled.
func (d *fakeDB) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for d.String() != want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := d.String(); got != want {
		t.Errorf("database saw:\n got: %s\nwant: %s", got, want)
	}
}

// open returns a *sql.DB backed by the fake, closed with the test.
func (d *fakeDB) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(fakeConnector{db: d})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type fakeConnector struct{ db *fakeDB }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{db: c.db}, nil }
func (c fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fake driver: open through the connector")
}

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake driver: statements are not supported")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.db.beginErr != nil {
		return nil, c.db.beginErr
	}
	if opts.ReadOnly {
		c.db.record("begin read-only")
	} else {
		c.db.record("begin")
	}
	return fakeTx{db: c.db}, nil
}

func (c *fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.db.record("exec " + query)
	return driver.RowsAffected(1), nil
}

type fakeTx struct{ db *fakeDB }

func (t fakeTx) Commit() error {
	if t.db.commitErr != nil {
		t.db.record("commit failed")
		return t.db.commitErr
	}
	t.db.record("commit")
	return nil
}

func (t fakeTx) Rollback() error {
	t.db.record("rollback")
	return nil
}

// txIn reads the transaction as a Dep, with a query parameter that can fail to
// bind.
type txIn struct {
	Tx       Dep[*sql.Tx]
	Quantity int `query:"quantity"`
}

// insertOrder writes through the request's transaction and then does what the
// request asks: succeed, fail, panic or wait for the client to leave.
func insertOrder(ctx *Context, in txIn) (relOut, error) {
	if _, err := in.Tx.Get().ExecContext(ctx.Context(), "insert"); err != nil {
		return relOut{}, err
	}
	if From[*sql.Tx](ctx) != in.Tx.Get() {
		return relOut{}, errors.New("From and the Dep disagree")
	}
	switch ctx.Query("then") {
	case "fail":
		return relOut{}, Conflict("out of stock")
	case "panic":
		panic("handler bug")
	}
	return relOut{Value: "placed"}, nil
}

func newTxApp(t *testing.T, fake *fakeDB, opts ...RouteOption) *App {
	t.Helper()
	app := New(quietOptions())
	app.Post("/orders", insertOrder, append([]RouteOption{Transaction(fake.open(t), nil)}, opts...)...)
	return mustBuild(t, app)
}

func TestTransactionCommitsWhenTheRequestSucceeds(t *testing.T) {
	t.Parallel()
	fake := &fakeDB{}
	app := newTxApp(t, fake)
	rec := do(t, app, http.MethodPost, "/orders")
	assertStatus(t, rec, http.StatusOK)
	assertJSON(t, rec, `{"value":"placed"}`)
	fake.waitFor(t, "begin, exec insert, commit")
}

func TestTransactionPassesItsOptions(t *testing.T) {
	t.Parallel()
	fake := &fakeDB{}
	app := New(quietOptions())
	app.Get("/report", func(ctx *Context, _ Empty) (relOut, error) {
		return relOut{Value: "ok"}, nil
	}, Transaction(fake.open(t), &sql.TxOptions{ReadOnly: true}))
	mustBuild(t, app)
	assertStatus(t, do(t, app, http.MethodGet, "/report"), http.StatusOK)
	fake.waitFor(t, "begin read-only, commit")
}

func TestTransactionRollsBackWhenTheRequestFails(t *testing.T) {
	t.Parallel()
	refuse := Needs(func(*Context) (depUser, error) { return depUser{}, Forbidden("not yours") })
	tests := []struct {
		name   string
		target string
		opts   []RouteOption
		status int
		events string
	}{
		{"the handler returns an error", "/orders?then=fail", nil, http.StatusConflict, "begin, exec insert, rollback"},
		{"the handler panics", "/orders?then=panic", nil, http.StatusInternalServerError, "begin, exec insert, rollback"},
		{"the input does not bind", "/orders?quantity=lots", nil, http.StatusUnprocessableEntity, "begin, rollback"},
		{"a later provider refuses", "/orders", []RouteOption{refuse}, http.StatusForbidden, "begin, rollback"},
		// A guard runs before every provider, so nothing is begun at all.
		{"a guard refuses", "/orders", []RouteOption{WithDependencies(func(*Context) error { return Unauthorized("no") })}, http.StatusUnauthorized, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeDB{}
			app := newTxApp(t, fake, tc.opts...)
			assertStatus(t, do(t, app, http.MethodPost, tc.target), tc.status)
			fake.waitFor(t, tc.events)
		})
	}
}

func TestTransactionCommitFailureIsAnOpaque500(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	fake := &fakeDB{commitErr: errors.New("could not serialize access; host=db.internal password=hunter2")}
	app := New(opts)
	app.Post("/orders", insertOrder, Transaction(fake.open(t), nil))
	mustBuild(t, app)

	rec := do(t, app, http.MethodPost, "/orders")
	assertStatus(t, rec, http.StatusInternalServerError)
	if body := rec.Body.String(); strings.Contains(body, "serialize") || strings.Contains(body, "hunter2") || strings.Contains(body, "placed") {
		t.Errorf("the response says more than that the request failed: %s", body)
	}
	if out := logs.String(); !strings.Contains(out, "committing the request's transaction failed") || !strings.Contains(out, "could not serialize access") {
		t.Errorf("the cause was not logged:\n%s", out)
	}
	fake.waitFor(t, "begin, exec insert, commit failed")
}

func TestTransactionBeginFailureStopsTheRequest(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	fake := &fakeDB{beginErr: errors.New("too many connections for role app")}
	ran := false
	app := New(opts)
	app.Post("/orders", func(*Context, txIn) (relOut, error) {
		ran = true
		return relOut{}, nil
	}, Transaction(fake.open(t), nil))
	mustBuild(t, app)

	rec := do(t, app, http.MethodPost, "/orders")
	assertStatus(t, rec, http.StatusInternalServerError)
	if ran {
		t.Error("the handler ran without a transaction")
	}
	if strings.Contains(rec.Body.String(), "too many connections") {
		t.Errorf("the driver's error reached the client: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "beginning the request's transaction failed") {
		t.Errorf("the cause was not logged:\n%s", logs.String())
	}
	fake.waitFor(t, "")
}

// TestTransactionIsNotCommittedForAClientThatLeft covers both ways a handler
// can meet a cancelled request: returning the context's error, and carrying on
// as though nothing happened. Neither commits.
func TestTransactionIsNotCommittedForAClientThatLeft(t *testing.T) {
	t.Parallel()
	for _, returnsErr := range []bool{true, false} {
		fake := &fakeDB{}
		app := New(quietOptions())
		app.Post("/orders", func(ctx *Context, in txIn) (relOut, error) {
			if _, err := in.Tx.Get().ExecContext(ctx.Context(), "insert"); err != nil {
				return relOut{}, err
			}
			cancelRequest(ctx)
			<-ctx.Context().Done()
			if returnsErr {
				return relOut{}, ctx.Context().Err()
			}
			return relOut{Value: "placed"}, nil
		}, Transaction(fake.open(t), nil))
		mustBuild(t, app)

		ctx, cancel := context.WithCancel(t.Context())
		req := httptest.NewRequestWithContext(context.WithValue(ctx, cancelKey{}, cancel), http.MethodPost, "/orders", nil)
		rec := doRequest(t, app, req)
		if rec.Code == http.StatusOK {
			t.Errorf("returnsErr=%v: status 200 for a transaction that was never committed", returnsErr)
		}
		fake.waitFor(t, "begin, exec insert, rollback")
	}
}

// cancelKey carries a request's cancel function to its handler, standing in
// for a client that disconnects while the handler runs.
type cancelKey struct{}

func cancelRequest(ctx *Context) {
	ctx.Context().Value(cancelKey{}).(context.CancelFunc)()
}

func TestTransactionWithANilDatabaseIsABuildError(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Post("/orders", insertOrder, Transaction(nil, nil))
	if err := app.Build(); err == nil || !strings.Contains(err.Error(), "muzak: Transaction was given a nil *sql.DB") {
		t.Errorf("Build() = %v, want the nil database reported", err)
	}
}

func TestTransactionLeavesNoGoroutineBehind(t *testing.T) {
	fake := &fakeDB{}
	app := newTxApp(t, fake)
	for range 20 {
		for _, target := range []string{"/orders", "/orders?then=fail", "/orders?then=panic", "/orders?quantity=x"} {
			do(t, app, http.MethodPost, target)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		doRequest(t, app, httptest.NewRequestWithContext(ctx, http.MethodPost, "/orders", nil))
	}
	assertNoGoroutineLeaks(t)
}

// TestEndTransactionIgnoresATransactionAlreadyEnded pins the one rollback
// error that is not one.
func TestEndTransactionIgnoresATransactionAlreadyEnded(t *testing.T) {
	t.Parallel()
	fake := &fakeDB{}
	tx, err := fake.open(t).BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := endTransaction(tx, errors.New("failed")); err != nil {
		t.Errorf("rolling back a finished transaction = %v, want nil", err)
	}
	if err := endTransaction(tx, nil); err == nil || !strings.Contains(err.Error(), "committing") {
		t.Errorf("committing a finished transaction = %v, want the failure", err)
	}
}

// TestEndTransactionReportsAFailedRollback covers the driver refusing the
// rollback itself, which is logged since the request is already failing.
func TestEndTransactionReportsAFailedRollback(t *testing.T) {
	t.Parallel()
	db := sql.OpenDB(rollbackFailingConnector{})
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := endTransaction(tx, errors.New("failed")); err == nil || !strings.Contains(err.Error(), "rolling back the request's transaction failed") {
		t.Errorf("endTransaction = %v, want the rollback failure", err)
	}
}

type rollbackFailingConnector struct{}

func (rollbackFailingConnector) Connect(context.Context) (driver.Conn, error) {
	return rollbackFailingConn{}, nil
}
func (rollbackFailingConnector) Driver() driver.Driver { return fakeDriver{} }

type rollbackFailingConn struct{}

func (rollbackFailingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake driver: statements are not supported")
}
func (rollbackFailingConn) Close() error              { return nil }
func (rollbackFailingConn) Begin() (driver.Tx, error) { return rollbackFailingTx{}, nil }

type rollbackFailingTx struct{}

func (rollbackFailingTx) Commit() error   { return nil }
func (rollbackFailingTx) Rollback() error { return errors.New("connection reset") }
