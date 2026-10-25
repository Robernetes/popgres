package cluster_test

import (
	"testing"
	"time"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/pocketbase/dbx"
)

func newTestKV(t *testing.T) (*cluster.KV, *dbx.DB) {
	testDB, db := openTestDB(t)

	// the same definition as the popgres cluster system migration
	_, err := db.NewQuery(`
		CREATE UNLOGGED TABLE {{_kv_ttl}} (
			[[key]]        TEXT PRIMARY KEY NOT NULL,
			[[value]]      TEXT DEFAULT '' NOT NULL,
			[[expires_at]] TIMESTAMPTZ NOT NULL
		)
	`).Execute()
	if err != nil {
		t.Fatal(err)
	}

	return newTestClusterWithDB(t, testDB.URL, db).KV(), db
}

func TestKVSetGetDelete(t *testing.T) {
	t.Parallel()

	kv, _ := newTestKV(t)

	if _, ok, err := kv.Get("missing"); err != nil || ok {
		t.Fatalf("Expected missing key, got ok %v (%v)", ok, err)
	}

	if err := kv.Set("a", "1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := kv.Set("a", "2", time.Minute); err != nil {
		t.Fatal(err)
	}

	value, ok, err := kv.Get("a")
	if err != nil || !ok || value != "2" {
		t.Fatalf("Expected value 2, got %q, %v (%v)", value, ok, err)
	}

	if !kv.Has("a") {
		t.Fatal("Expected key a to exist")
	}

	if err := kv.Delete("a"); err != nil {
		t.Fatal(err)
	}

	if kv.Has("a") {
		t.Fatal("Expected key a to be deleted")
	}
}

func TestKVExpiration(t *testing.T) {
	t.Parallel()

	kv, db := newTestKV(t)

	if err := kv.Set("expired", "1", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := kv.Set("active", "1", time.Minute); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)

	if kv.Has("expired") {
		t.Fatal("Expected the expired key to be missing")
	}

	// an expired key can be claimed again
	claimed, err := kv.Claim("expired", "2", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("Expected the expired key to be claimed, got %v (%v)", claimed, err)
	}

	if err := kv.Set("expired2", "1", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	if _, ok, err := kv.Pop("expired2"); err != nil || ok {
		t.Fatalf("Expected no value for an expired key pop, got %v (%v)", ok, err)
	}

	if err := kv.Set("expired3", "1", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	if err := kv.DeleteExpired(); err != nil {
		t.Fatal(err)
	}

	var keys []string
	if err := db.Select("key").From(cluster.KVTable).OrderBy("key").Column(&keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "active" || keys[1] != "expired" {
		t.Fatalf("Expected only the active keys to remain, got %v", keys)
	}
}

func TestKVClaim(t *testing.T) {
	t.Parallel()

	kv, _ := newTestKV(t)

	const total = 10

	results := make(chan bool, total)
	for range total {
		go func() {
			claimed, err := kv.Claim("job", "node", time.Minute)
			if err != nil {
				t.Error(err)
			}
			results <- claimed
		}()
	}

	var claims int
	for range total {
		if <-results {
			claims++
		}
	}

	if claims != 1 {
		t.Fatalf("Expected exactly 1 claim, got %d", claims)
	}
}

func TestKVPop(t *testing.T) {
	t.Parallel()

	kv, _ := newTestKV(t)

	if err := kv.Set("name", "John", time.Minute); err != nil {
		t.Fatal(err)
	}

	value, ok, err := kv.Pop("name")
	if err != nil || !ok || value != "John" {
		t.Fatalf("Expected John, got %q, %v (%v)", value, ok, err)
	}

	if _, ok, err := kv.Pop("name"); err != nil || ok {
		t.Fatalf("Expected the key to be popped only once, got %v (%v)", ok, err)
	}
}
