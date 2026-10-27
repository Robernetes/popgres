package core

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pocketbase/dbx"
)

// CollectionDDLLockTimeout is the max time that a collection change waits for a table lock
// (eg. of a collection table used by a long running direct SQL query or transaction).
//
// The schema and RLS policies changes need an ACCESS EXCLUSIVE lock of the table and
// all other queries of the table wait behind it, so the collection change fails instead.
const CollectionDDLLockTimeout = 5 * time.Second

// withDDLLockTimeout wraps a collection change transaction function so that
// its statements wait at most [CollectionDDLLockTimeout] for the table locks
// (unless a lock_timeout is already configured, eg. for the app role).
func withDDLLockTimeout(fn func(txApp App) error) func(txApp App) error {
	return func(txApp App) error {
		var current string
		if err := txApp.DB().NewQuery("SELECT current_setting('lock_timeout')").Row(&current); err != nil {
			return err
		}

		if current != "0" {
			return fn(txApp)
		}

		setLockTimeout := func(value string) error {
			_, err := txApp.DB().NewQuery("SELECT set_config('lock_timeout', {:value}, true)").
				Bind(dbx.Params{"value": value}).
				Execute()
			return err
		}

		if err := setLockTimeout(strconv.FormatInt(CollectionDDLLockTimeout.Milliseconds(), 10)); err != nil {
			return err
		}

		if err := fn(txApp); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" { // lock_not_available
				return fmt.Errorf("the collection table is locked by another transaction (eg. a long running direct SQL query), try again later: %w", err)
			}
			return err
		}

		// restore the default for the rest of the transaction (eg. a migration)
		return setLockTimeout("0")
	}
}
