package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pocketbase/dbx"
)

// default retries intervals (in ms)
var defaultRetryIntervals = []int{50, 100, 150, 200, 300, 400, 500, 700, 1000}

// default max retry attempts
const defaultMaxLockRetries = 12

// Postgres error codes (SQLSTATE) of the transient errors that are safe to retry
// when the failed statement is not part of an already aborted transaction.
var retryableSQLStates = []string{
	"40001", // serialization_failure
	"40P01", // deadlock_detected
	"55P03", // lock_not_available
}

// sqlStateInFailedTransaction is the code of the error returned for any
// statement executed in an already aborted transaction.
const sqlStateInFailedTransaction = "25P02"

func execLockRetry(timeout time.Duration, maxRetries int) dbx.ExecHookFunc {
	return func(q *dbx.Query, op func() error) error {
		if q.Context() == nil {
			cancelCtx, cancel := context.WithTimeout(context.Background(), timeout)
			defer func() {
				cancel()
				//nolint:staticcheck
				q.WithContext(nil) // reset
			}()
			q.WithContext(cancelCtx)
		}

		execErr := baseLockRetry(func(attempt int) error {
			return op()
		}, maxRetries)
		if execErr != nil && !errors.Is(execErr, sql.ErrNoRows) {
			execErr = fmt.Errorf("%w; failed query: %s", execErr, q.SQL())
		}

		return execErr
	}
}

func baseLockRetry(op func(attempt int) error, maxRetries int) error {
	attempt := 1

	var firstErr error

Retry:
	err := op(attempt)

	// the operation was part of a transaction that was aborted by the previous error
	// and retrying it individually is not possible (the caller must retry the transaction)
	if firstErr != nil && sqlState(err) == sqlStateInFailedTransaction {
		return firstErr
	}

	if err != nil && attempt <= maxRetries && isRetryableDBError(err) {
		if firstErr == nil {
			firstErr = err
		}

		// wait and retry
		time.Sleep(getDefaultRetryInterval(attempt))
		attempt++
		goto Retry
	}

	return err
}

// isRetryableDBError reports whether err is a transient Postgres
// concurrency error (serialization failure, deadlock or lock timeout).
func isRetryableDBError(err error) bool {
	code := sqlState(err)
	for _, c := range retryableSQLStates {
		if code == c {
			return true
		}
	}

	return false
}

// sqlState returns the Postgres error code (SQLSTATE) of err or empty string if there is none.
func sqlState(err error) string {
	if err == nil {
		return ""
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}

	// fallback for drivers that don't expose a typed error but follow the pgx message format
	msg := err.Error()
	if i := strings.LastIndex(msg, "(SQLSTATE "); i >= 0 && len(msg) >= i+15 {
		return msg[i+10 : i+15]
	}

	return ""
}

func getDefaultRetryInterval(attempt int) time.Duration {
	if attempt < 0 || attempt > len(defaultRetryIntervals)-1 {
		return time.Duration(defaultRetryIntervals[len(defaultRetryIntervals)-1]) * time.Millisecond
	}

	return time.Duration(defaultRetryIntervals[attempt]) * time.Millisecond
}
