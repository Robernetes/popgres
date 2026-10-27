package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
)

func TestCollectionDDLLockTimeout(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	ctx := context.Background()

	// a long running transaction that reads the collection table (eg. of a direct SQL client)
	conn, err := app.DB().(*dbx.DB).DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT * FROM "demo1" LIMIT 1`); err != nil {
		t.Fatal(err)
	}

	collection, err := app.FindCollectionByNameOrId("demo1")
	if err != nil {
		t.Fatal(err)
	}
	collection.Fields.Add(&core.TextField{Name: "lock_test"})

	start := time.Now()
	err = app.Save(collection)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "locked by another transaction") {
		t.Fatalf("Expected a lock error, got %v", err)
	}
	if elapsed > core.CollectionDDLLockTimeout+5*time.Second {
		t.Fatalf("Expected the save to fail after about %s, got %s", core.CollectionDDLLockTimeout, elapsed)
	}

	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	// the save works after the transaction ends
	collection, err = app.FindCollectionByNameOrId("demo1")
	if err != nil {
		t.Fatal(err)
	}
	collection.Fields.Add(&core.TextField{Name: "lock_test"})
	if err := app.Save(collection); err != nil {
		t.Fatalf("Expected the save to succeed, got %v", err)
	}
}

func TestCollectionDDLLockTimeoutRestore(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	err := app.RunInTransaction(func(txApp core.App) error {
		collection, err := txApp.FindCollectionByNameOrId("demo1")
		if err != nil {
			return err
		}
		collection.Fields.Add(&core.TextField{Name: "lock_test"})
		if err := txApp.Save(collection); err != nil {
			return err
		}

		// the lock_timeout is restored for the rest of the transaction
		var lockTimeout string
		if err := txApp.DB().NewQuery("SELECT current_setting('lock_timeout')").Row(&lockTimeout); err != nil {
			return err
		}
		if lockTimeout != "0" {
			t.Fatalf("Expected lock_timeout 0 after the collection save, got %q", lockTimeout)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
