package cluster_test

import (
	"context"
	"testing"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/pocketbase/dbx"
)

func TestLockKey(t *testing.T) {
	t.Parallel()

	if cluster.LockKey("a") != cluster.LockKey("a") {
		t.Fatal("Expected the same key for the same name")
	}

	if cluster.LockKey("a") == cluster.LockKey("b") {
		t.Fatal("Expected different keys for different names")
	}
}

func TestXactLock(t *testing.T) {
	t.Parallel()

	_, db := openTestDB(t)

	err := db.Transactional(func(tx *dbx.Tx) error {
		return cluster.XactLock(tx, "test")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTryLock(t *testing.T) {
	t.Parallel()

	testDB, db := openTestDB(t)

	// two nodes sharing the same database
	a := newTestClusterWithDB(t, testDB.URL, db)
	b := newTestClusterWithDB(t, testDB.URL, db)

	ctx := context.Background()

	for _, name := range []string{"test", "other"} {
		locked, err := b.IsLocked(ctx, name)
		if err != nil || locked {
			t.Fatalf("[%s] Expected the lock to be free, got %v (%v)", name, locked, err)
		}
	}

	unlock, err := a.TryLock(ctx, "test")
	if err != nil || unlock == nil {
		t.Fatalf("Expected the lock to be acquired, got %v", err)
	}

	locked, err := b.IsLocked(ctx, "test")
	if err != nil || !locked {
		t.Fatalf("Expected the lock to be reported as held, got %v (%v)", locked, err)
	}

	locked, err = b.IsLocked(ctx, "other")
	if err != nil || locked {
		t.Fatalf("Expected the other lock to be free, got %v (%v)", locked, err)
	}

	unlock2, err := b.TryLock(ctx, "test")
	if err != nil || unlock2 != nil {
		t.Fatalf("Expected the lock to be held by the other node, got %v", err)
	}

	unlock()

	locked, err = b.IsLocked(ctx, "test")
	if err != nil || locked {
		t.Fatalf("Expected the lock to be released, got %v (%v)", locked, err)
	}

	unlock3, err := b.TryLock(ctx, "test")
	if err != nil || unlock3 == nil {
		t.Fatalf("Expected the released lock to be acquired, got %v", err)
	}
	unlock3()
}
