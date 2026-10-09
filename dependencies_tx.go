package muzak

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
)

// Transaction declares a request-scoped *sql.Tx, begun before the handler runs
// and committed only if the request succeeds:
//
//	r.Post("/orders", func(ctx *muzak.Context, in PlaceOrder) (OrderOut, error) {
//		tx := in.Tx.Get()
//		if _, err := tx.ExecContext(ctx.Context(), insertOrder, in.Item); err != nil {
//			return OrderOut{}, err
//		}
//		return OrderOut{Item: in.Item}, nil
//	}, muzak.Transaction(db, nil))
//
// where PlaceOrder holds `Tx muzak.Dep[*sql.Tx]`; [From] reads it too.
//
// It is an [Acquire] provider, so everything Acquire says about when its
// release runs holds. The transaction is begun with the request's context and
// the given options, which may be nil. It is committed when the handler, and
// everything after it, succeeded: the handler returned no error, the response
// encoded, and no release acquired after it failed. It is rolled back
// otherwise, including when the handler panicked, a later provider or the rate
// limit refused the request, binding or validation failed, or the client went
// away. A rollback that finds the transaction already ended, which
// database/sql does itself when the request's context is cancelled, is not an
// error.
//
// A commit that fails turns the response into a 500 whose cause is logged and
// never sent, because the client must not be told that work it asked for was
// done when it was not. A transaction that cannot be begun fails the request
// the same way, before the handler runs.
//
// A transaction spanning an event stream or a WebSocket commits only if the
// handler returns nil: a stream the client left, or a connection the peer
// closed, ends with an error, and is rolled back. Return nil to keep the work.
//
// Declare it where the writes are, not on the whole application: a file mount
// and the documentation run the providers they inherit, and would begin a
// transaction for every file they serve.
func Transaction(db *sql.DB, opts *sql.TxOptions) SharedOption {
	p := &provider{typ: reflect.TypeFor[*sql.Tx]()}
	if db == nil {
		p.invalid = errors.New("muzak: Transaction was given a nil *sql.DB")
	}
	p.resolve = acquiring(p.typ, func(c *Context) (*sql.Tx, Release, error) {
		tx, err := db.BeginTx(c.Context(), opts)
		if err != nil {
			return nil, nil, fmt.Errorf("muzak: beginning the request's transaction failed: %w", err)
		}
		return tx, func(failure error) error { return endTransaction(tx, failure) }, nil
	})
	return providerOption(p)
}

// endTransaction commits a transaction when failure is nil and rolls it back
// otherwise.
func endTransaction(tx *sql.Tx, failure error) error {
	if failure != nil {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			return fmt.Errorf("muzak: rolling back the request's transaction failed: %w", err)
		}
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("muzak: committing the request's transaction failed: %w", err)
	}
	return nil
}
