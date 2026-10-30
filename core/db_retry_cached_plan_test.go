package core_test

import (
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
)

// TestRecordsQueryAfterFieldChange checks that a records query prepared on a
// connection before a collection field change is retried after the change
// ("cached plan must not change result type").
func TestRecordsQueryAfterFieldChange(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("cached_plan")
	collection.Fields.Add(&core.TextField{Name: "title"})
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := app.FindRecordsByFilter("cached_plan", "title = 'a'", "", 0, 0); err != nil {
			t.Fatal(err)
		}
	}

	collection.Fields.Add(&core.TextField{Name: "extra"})
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := app.FindRecordsByFilter("cached_plan", "title = 'a'", "", 0, 0); err != nil {
			t.Fatalf("[%d] Expected the query to succeed after the field change, got %v", i, err)
		}
	}
}
