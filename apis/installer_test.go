package apis_test

import (
	"testing"

	"github.com/Robernetes/popgres/apis"
	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
)

func TestFindOrCreateInstallerSuperuserConcurrently(t *testing.T) {
	t.Parallel()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	const total = 5

	type result struct {
		id  string
		err error
	}

	results := make(chan result, total)
	for range total {
		go func() {
			record, err := apis.FindOrCreateInstallerSuperuser(app)
			if err != nil {
				results <- result{err: err}
				return
			}
			results <- result{id: record.Id}
		}()
	}

	var id string
	for range total {
		r := <-results
		if r.err != nil {
			t.Fatalf("Failed to find or create the installer superuser: %v", r.err)
		}
		if id == "" {
			id = r.id
		} else if id != r.id {
			t.Fatalf("Expected the same installer superuser, got %q and %q", id, r.id)
		}
	}

	count, err := app.CountRecords(core.CollectionNameSuperusers, dbx.HashExp{"email": core.DefaultInstallerEmail})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("Expected 1 installer superuser, got %d", count)
	}
}
