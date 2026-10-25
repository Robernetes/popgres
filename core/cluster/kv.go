package cluster

import (
	"database/sql"
	"errors"
	"time"

	"github.com/pocketbase/dbx"
)

// KVTable is the UNLOGGED table of the shared keys (created by the system migrations).
const KVTable = "_kv_ttl"

// KV is a key-value store with expiration that is shared by all nodes.
//
// It replaces the app in-memory store for the temporary state that must be
// visible to all nodes (eg. the resend timeouts of the auth emails).
//
// The queries are executed with the main database pool (and not with the
// current transaction, if any) because the state is not part of the
// transaction data and it is usually written after the transaction completion.
type KV struct {
	db func() dbx.Builder
}

func newKV(db func() dbx.Builder) *KV {
	return &KV{db: db}
}

func (kv *KV) builder() (dbx.Builder, error) {
	if kv.db == nil {
		return nil, errors.New("missing cluster database")
	}

	db := kv.db()
	if db == nil {
		return nil, errors.New("the cluster database is not initialized")
	}

	return db, nil
}

// Set stores the value of the key (replacing the existing one) for the ttl duration.
func (kv *KV) Set(key string, value string, ttl time.Duration) error {
	db, err := kv.builder()
	if err != nil {
		return err
	}

	_, err = db.NewQuery(`
		INSERT INTO {{_kv_ttl}} ([[key]], [[value]], [[expires_at]])
		VALUES ({:key}, {:value}, now() + make_interval(secs => {:ttl}))
		ON CONFLICT ([[key]]) DO UPDATE SET
			[[value]] = EXCLUDED.[[value]],
			[[expires_at]] = EXCLUDED.[[expires_at]]
	`).Bind(dbx.Params{
		"key":   key,
		"value": value,
		"ttl":   ttl.Seconds(),
	}).Execute()

	return err
}

// Claim stores the value of the key for the ttl duration only if the key
// doesn't exist or has expired.
//
// It reports whether the key was stored (aka. claimed by the caller).
func (kv *KV) Claim(key string, value string, ttl time.Duration) (bool, error) {
	db, err := kv.builder()
	if err != nil {
		return false, err
	}

	var claimed string

	err = db.NewQuery(`
		INSERT INTO {{_kv_ttl}} ([[key]], [[value]], [[expires_at]])
		VALUES ({:key}, {:value}, now() + make_interval(secs => {:ttl}))
		ON CONFLICT ([[key]]) DO UPDATE SET
			[[value]] = EXCLUDED.[[value]],
			[[expires_at]] = EXCLUDED.[[expires_at]]
		WHERE {{_kv_ttl}}.[[expires_at]] <= now()
		RETURNING [[key]]
	`).Bind(dbx.Params{
		"key":   key,
		"value": value,
		"ttl":   ttl.Seconds(),
	}).Row(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

// Get returns the value of the key and whether it exists (the expired keys don't exist).
func (kv *KV) Get(key string) (string, bool, error) {
	db, err := kv.builder()
	if err != nil {
		return "", false, err
	}

	var value string

	err = db.NewQuery(`
		SELECT [[value]] FROM {{_kv_ttl}}
		WHERE [[key]] = {:key} AND [[expires_at]] > now()
	`).Bind(dbx.Params{"key": key}).Row(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	return value, true, nil
}

// Has reports whether the key exists (errors are reported as missing key).
func (kv *KV) Has(key string) bool {
	_, ok, _ := kv.Get(key)
	return ok
}

// Pop returns the value of the key and deletes it (it is like Get followed by Delete
// but only one of the concurrent callers receives the value).
func (kv *KV) Pop(key string) (string, bool, error) {
	db, err := kv.builder()
	if err != nil {
		return "", false, err
	}

	// the value is NULL for an expired key
	var value sql.NullString

	err = db.NewQuery(`
		DELETE FROM {{_kv_ttl}}
		WHERE [[key]] = {:key}
		RETURNING CASE WHEN [[expires_at]] > now() THEN [[value]] END
	`).Bind(dbx.Params{"key": key}).Row(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	return value.String, value.Valid, nil
}

// Delete deletes the key (if exists).
func (kv *KV) Delete(key string) error {
	db, err := kv.builder()
	if err != nil {
		return err
	}

	_, err = db.NewQuery("DELETE FROM {{_kv_ttl}} WHERE [[key]] = {:key}").
		Bind(dbx.Params{"key": key}).
		Execute()

	return err
}

// DeleteExpired deletes all expired keys.
func (kv *KV) DeleteExpired() error {
	db, err := kv.builder()
	if err != nil {
		return err
	}

	_, err = db.NewQuery("DELETE FROM {{_kv_ttl}} WHERE [[expires_at]] <= now()").Execute()

	return err
}
