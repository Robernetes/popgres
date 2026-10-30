package core_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
)

func TestSearchFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, core.FieldTypeSearch)
}

func TestSearchFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		field    *core.SearchField
		expected string
	}{
		{
			&core.SearchField{},
			"tsvector GENERATED ALWAYS AS (''::tsvector) STORED",
		},
		{
			&core.SearchField{Fields: []string{"title"}},
			`tsvector GENERATED ALWAYS AS (setweight(to_tsvector('simple'::regconfig, COALESCE("title"::text, '')), 'A')) STORED`,
		},
		{
			&core.SearchField{Fields: []string{"a", "b", "c", "d", "e"}, Language: "english"},
			`tsvector GENERATED ALWAYS AS (` +
				`setweight(to_tsvector('english'::regconfig, COALESCE("a"::text, '')), 'A') || ` +
				`setweight(to_tsvector('english'::regconfig, COALESCE("b"::text, '')), 'B') || ` +
				`setweight(to_tsvector('english'::regconfig, COALESCE("c"::text, '')), 'C') || ` +
				`setweight(to_tsvector('english'::regconfig, COALESCE("d"::text, '')), 'D') || ` +
				`setweight(to_tsvector('english'::regconfig, COALESCE("e"::text, '')), 'D')) STORED`,
		},
	}

	for _, s := range scenarios {
		if v := s.field.ColumnType(app); v != s.expected {
			t.Fatalf("Expected\n%s\ngot\n%s", s.expected, v)
		}
	}
}

func TestSearchFieldPrepareAndValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &core.SearchField{Name: "fts", Fields: []string{"title"}}
	record := core.NewRecord(core.NewBaseCollection("test"))

	v, err := f.PrepareValue(record, "'abc':1A")
	if err != nil || v != nil {
		t.Fatalf("Expected nil value, got %v (%v)", v, err)
	}

	if err := f.ValidateValue(context.Background(), app, record); err != nil {
		t.Fatalf("Expected no validation error, got %v", err)
	}
}

func TestSearchFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, core.FieldTypeSearch)
	testDefaultFieldNameValidation(t, core.FieldTypeSearch)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	newCollection := func() *core.Collection {
		collection := core.NewBaseCollection("test_collection")
		collection.Fields.Add(
			&core.TextField{Name: "title"},
			&core.EditorField{Name: "body"},
			&core.SelectField{Name: "tags", Values: []string{"a", "b"}, MaxSelect: 2},
			&core.NumberField{Name: "total"},
			&core.TextField{Name: "secret", Hidden: true},
		)
		return collection
	}

	scenarios := []struct {
		name         string
		collection   func() *core.Collection
		field        *core.SearchField
		expectErrors []string
	}{
		{
			"zero minimal",
			newCollection,
			&core.SearchField{Id: "test", Name: "test"},
			[]string{"fields"},
		},
		{
			"missing source field",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"title", "missing"}},
			[]string{"fields"},
		},
		{
			"unsupported source field type",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"total"}},
			[]string{"fields"},
		},
		{
			"self as source",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"test"}},
			[]string{"fields"},
		},
		{
			"duplicated source fields",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"title", "title"}},
			[]string{"fields"},
		},
		{
			"too many source fields",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
			[]string{"fields"},
		},
		{
			"hidden source field with non-hidden search field",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"title", "secret"}},
			[]string{"fields"},
		},
		{
			"hidden source field with hidden search field",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"title", "secret"}, Hidden: true},
			[]string{},
		},
		{
			"auth email source field",
			func() *core.Collection { return core.NewAuthCollection("test_auth") },
			&core.SearchField{Id: "test", Name: "test", Fields: []string{core.FieldNameEmail}},
			[]string{"fields"},
		},
		{
			"invalid language name",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"title"}, Language: "english'; --"},
			[]string{"language"},
		},
		{
			"unknown language",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"title"}, Language: "klingon"},
			[]string{"language"},
		},
		{
			"valid",
			newCollection,
			&core.SearchField{Id: "test", Name: "test", Fields: []string{"title", "body", "tags"}, Language: "spanish"},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			collection := s.collection()
			collection.Fields.Add(s.field)

			errs := s.field.ValidateSettings(context.Background(), app, collection)

			tests.TestValidationErrors(t, errs, s.expectErrors)
		})
	}
}

// searchTitles returns the titles of the posts records matching the provided filter and sort.
func searchTitles(t *testing.T, app core.App, filter string, sort string) []string {
	t.Helper()

	records, err := app.FindRecordsByFilter("posts", filter, sort, 0, 0)
	if err != nil {
		t.Fatalf("Failed to search %q: %v", filter, err)
	}

	titles := make([]string, len(records))
	for i, r := range records {
		titles[i] = r.GetString("title") + r.GetString("heading")
	}

	return titles
}

func searchColumnAttnum(t *testing.T, app core.App, table string, column string) int {
	t.Helper()

	var attnum int
	err := app.DB().NewQuery(
		"SELECT attnum FROM pg_attribute WHERE attrelid = {:table}::regclass AND attname = {:column} AND NOT attisdropped",
	).Bind(map[string]any{"table": table, "column": column}).Row(&attnum)
	if err != nil {
		t.Fatalf("Failed to find the %s.%s column: %v", table, column, err)
	}

	return attnum
}

// TestSearchFieldRecordsAndSync checks the generated column, the GIN index,
// the filter and sort and the table changes with a real collection.
func TestSearchFieldRecordsAndSync(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("posts")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.EditorField{Name: "body"},
		&core.SelectField{Name: "tag", Values: []string{"news", "sports"}, MaxSelect: 1},
		&core.SearchField{Name: "fts", Fields: []string{"title", "body"}, Language: "english"},
	)
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	field := collection.Fields.GetByName("fts").(*core.SearchField)

	var indexDef string
	err := app.DB().NewQuery("SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_fts_" + collection.Id + "_" + field.Id + "'").Row(&indexDef)
	if err != nil || !strings.Contains(indexDef, "gin") {
		t.Fatalf("Expected the GIN index, got %q (%v)", indexDef, err)
	}

	for _, data := range []map[string]any{
		{"title": "Running shoes", "body": "<p>Light shoes for the <b>marathon</b></p>", "tag": "sports"},
		{"title": "Election news", "body": "<p>The runners of the election</p>", "tag": "news"},
		{"title": "Cooking", "body": "<p>Red peppers and shoes polish</p>"},
	} {
		record := core.NewRecord(collection)
		record.Load(data)
		record.Set("fts", "ignored")
		if err := app.Save(record); err != nil {
			t.Fatal(err)
		}
	}

	// the field is never exported
	record, err := app.FindFirstRecordByData("posts", "title", "Cooking")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := record.PublicExport()["fts"]; ok {
		t.Fatal("Expected the search field to not be exported")
	}
	record.Unhide("fts")
	if _, ok := record.PublicExport()["fts"]; ok {
		t.Fatal("Expected the search field to not be exported also for the unhidden fields")
	}

	// an update keeps the generated column in sync
	record.Set("title", "Cooking marathon")
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}

	equal := func(name string, got []string, expected ...string) {
		t.Helper()
		if !slices.Equal(got, expected) {
			t.Fatalf("[%s] Expected %v, got %v", name, expected, got)
		}
	}

	// english stemming ("runners" -> "runner", not "run") and the html tags are not words
	equal("stemming", searchTitles(t, app, "fts ~ 'running'", "title"), "Running shoes")
	equal("html", searchTitles(t, app, "fts ~ 'p'", "title"))
	equal("all words", searchTitles(t, app, "fts ~ 'marathon shoes'", "title"), "Cooking marathon", "Running shoes")
	equal("or", searchTitles(t, app, "fts ~ 'election or peppers'", "title"), "Cooking marathon", "Election news")
	equal("negation", searchTitles(t, app, "fts ~ 'shoes -running'", "title"), "Cooking marathon")
	equal("not", searchTitles(t, app, "fts !~ 'shoes'", "title"), "Election news")
	equal("empty", searchTitles(t, app, "fts ~ ''", "title"), "Cooking marathon", "Election news", "Running shoes")

	// the title (weight A) is more relevant than the body (weight B)
	equal("rank", searchTitles(t, app, "fts ~ 'shoes'", "-@textRank(fts, 'shoes'),title"), "Running shoes", "Cooking marathon")

	// a source field rename keeps the generated column (and updates the sources)
	attnum := searchColumnAttnum(t, app, "posts", "fts")
	collection.Fields.GetByName("title").SetName("heading")
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(field.Fields, []string{"heading", "body"}) {
		t.Fatalf("Expected the renamed source field, got %v", field.Fields)
	}
	if v := searchColumnAttnum(t, app, "posts", "fts"); v != attnum {
		t.Fatalf("Expected the generated column to be kept (attnum %d), got %d", attnum, v)
	}
	equal("renamed source", searchTitles(t, app, "fts ~ 'election'", "heading"), "Election news")

	// a removed source field can't be deleted while used
	collection.Fields.RemoveByName("body")
	if err := app.Save(collection); err == nil {
		t.Fatal("Expected a validation error for the removed source field")
	}

	// removed together from the sources
	field.Fields = []string{"heading", "tag"}
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	if v := searchColumnAttnum(t, app, "posts", "fts"); v == attnum {
		t.Fatal("Expected the generated column to be recreated")
	}
	equal("changed sources", searchTitles(t, app, "fts ~ 'sports'", ""), "Running shoes")
	equal("removed source", searchTitles(t, app, "fts ~ 'peppers'", ""))

	// a single to multiple select change of a source field
	collection.Fields.GetByName("tag").(*core.SelectField).MaxSelect = 2
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	equal("multiple select", searchTitles(t, app, "fts ~ 'news'", ""), "Election news")

	// renamed search field and language change
	field.Name = "content"
	field.Language = "simple"
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	equal("simple language", searchTitles(t, app, "content ~ 'running'", "heading"), "Running shoes")
	equal("simple language no stemming", searchTitles(t, app, "content ~ 'run'", "heading"))

	// removed search field
	collection.Fields.RemoveByName("content")
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	var total int
	err = app.DB().NewQuery("SELECT count(*) FROM pg_attribute WHERE attrelid = 'posts'::regclass AND attname = 'content' AND NOT attisdropped").Row(&total)
	if err != nil || total != 0 {
		t.Fatalf("Expected the search column to be dropped, got %d (%v)", total, err)
	}
}
