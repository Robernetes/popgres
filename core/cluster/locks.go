package cluster

import (
	"hash/fnv"

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
