package cluster

import (
	"context"
	"errors"
	"hash/fnv"
	"time"

	"github.com/pocketbase/dbx"
)

// LockKey returns the Postgres advisory lock key of the named lock.
//
// The advisory locks are per database, so the same name can be used
// by multiple popgres databases on the same server.
func LockKey(name string) int64 {
	h := fnv.New64a()
	h.Write([]byte("popgres:" + name))

	return int64(h.Sum64())
}

// XactLock acquires the named transaction level advisory lock, waiting
// if necessary. The lock is released at the end of the transaction.
//
// db is expected to be a transaction (otherwise the lock is released
// immediately after the statement).
func XactLock(db dbx.Builder, name string) error {
	_, err := db.NewQuery("SELECT pg_advisory_xact_lock({:key})").
		Bind(dbx.Params{"key": LockKey(name)}).
		Execute()

	return err
}

// TryLock tries to acquire the named session level advisory lock without waiting.
//
// On success it returns a function that releases the lock. The lock is held by
// a dedicated connection of the main pool until it is released (or until the
// connection is closed, eg. if the app instance crashes).
//
// It returns nil (and no error) if the lock is held by another session.
func (c *Cluster) TryLock(ctx context.Context, name string) (unlock func(), err error) {
	db, err := c.pool()
	if err != nil {
		return nil, err
	}

	conn, err := db.DB().Conn(ctx)
	if err != nil {
		return nil, err
	}

	var locked bool

	err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", LockKey(name)).Scan(&locked)
	if err != nil || !locked {
		_ = conn.Close()
		return nil, err
	}

	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// note: closing the connection would release the lock too
		// but the connection is returned to the pool for reuse
		_, unlockErr := conn.ExecContext(releaseCtx, "SELECT pg_advisory_unlock($1)", LockKey(name))
		if unlockErr != nil {
			c.logger().Warn("Failed to release the cluster lock", "name", name, "error", unlockErr)
		}

		_ = conn.Close()
	}, nil
}

// IsLocked reports whether the named session level advisory lock is held by a session.
func (c *Cluster) IsLocked(ctx context.Context, name string) (bool, error) {
	db, err := c.pool()
	if err != nil {
		return false, err
	}

	var locked bool

	err = db.NewQuery(`
		SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE [[locktype]] = 'advisory' AND [[granted]] AND [[database]] = (SELECT oid FROM pg_database WHERE [[datname]] = current_database())
			AND [[objsubid]] = 1 AND (([[classid]]::bigint << 32) | [[objid]]::bigint) = {:key}
		)
	`).WithContext(ctx).Bind(dbx.Params{"key": LockKey(name)}).Row(&locked)

	return locked, err
}

// pool returns the main database pool.
func (c *Cluster) pool() (*dbx.DB, error) {
	builder, err := c.kv.builder()
	if err != nil {
		return nil, err
	}

	db, ok := builder.(*dbx.DB)
	if !ok {
		return nil, errors.New("the cluster database is not a *dbx.DB pool")
	}

	return db, nil
}
