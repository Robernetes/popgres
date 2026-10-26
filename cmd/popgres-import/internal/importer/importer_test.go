package importer_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Robernetes/popgres/apis"
	"github.com/Robernetes/popgres/cmd/popgres-import/internal/importer"
	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	_ "modernc.org/sqlite"

	_ "github.com/Robernetes/popgres/migrations"
)

// superuserToken is the static auth token of the fixtures superuser
// (signed with its token key and collection secret, the same in both databases).
const superuserToken = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY"

func testsDataDir() string {
	_, currentFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(currentFile), "..", "..", "..", "..", "tests", "data")
}

// copyPBData copies the SQLite test data into a new temp pb_data directory.
//
// The shared tests/data directory is never opened with SQLite because it
// creates temporary -shm/-wal files next to the databases, which the other
// test packages (running in parallel) would copy with the test data.
func copyPBData(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for _, name := range []string{"data.db", "auxiliary.db"} {
		raw, err := os.ReadFile(filepath.Join(testsDataDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Mkdir(filepath.Join(dir, core.LocalStorageDirName), os.ModePerm); err != nil {
		t.Fatal(err)
	}

	return dir
}

// newEmptyApp bootstraps a new app with an empty test database (only the system migrations).
func newEmptyApp(t *testing.T) core.App {
	t.Helper()

	testDB, err := tests.NewTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tests.DropTestDatabase(testDB.Name) })

	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:          t.TempDir(),
		DatabaseURL:      testDB.URL,
		EncryptionEnv:    "pb_test_env",
		DataMaxOpenConns: 4,
		AuxMaxOpenConns:  2,
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.ClearBootstrap() })

	return app
}

// apiGet executes a superuser GET request through the app router and returns the decoded body.
func apiGet(t *testing.T, app core.App, url string) any {
	t.Helper()

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", superuserToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: expected 200, got %d\n%s", url, rec.Code, rec.Body.String())
	}

	var result any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	return result
}

// compareAPIs compares the collections, records, settings and logs of the two
// apps through the API (the views that failed to import are skipped).
func compareAPIs(t *testing.T, imported core.App, reference core.App, skip map[string]error) {
	t.Helper()

	collections, err := reference.FindAllCollections()
	if err != nil {
		t.Fatal(err)
	}

	// note: the test apps disable the request logs in memory (and the
	// reference fixtures app removed the old logs on bootstrap), so the
	// imported logs are checked separately
	imported.Settings().Logs.MaxDays = reference.Settings().Logs.MaxDays

	urls := []string{"/api/settings"}
	for _, c := range collections {
		if _, ok := skip[c.Name]; ok {
			continue
		}
		urls = append(urls, "/api/collections/"+c.Name)
		urls = append(urls, "/api/collections/"+c.Name+"/records?perPage=500&sort=id")
	}

	for _, url := range urls {
		got := apiGet(t, imported, url)
		expected := apiGet(t, reference, url)

		if !reflect.DeepEqual(got, expected) {
			gotRaw, _ := json.Marshal(got)
			expectedRaw, _ := json.Marshal(expected)
			t.Errorf("GET %s differs from the fixtures:\nexpected %s\ngot      %s", url, truncate(expectedRaw), truncate(gotRaw))
		}
	}
}

func truncate(raw []byte) string {
	if len(raw) > 1500 {
		return string(raw[:1500]) + "..."
	}
	return string(raw)
}

// TestImportFixtures imports the PocketBase test data (SQLite) into an empty
// database and compares the result through the API with the test fixtures
// (the collections, the records of all collections, including the views, and the settings).
func TestImportFixtures(t *testing.T) {
	imported := newEmptyApp(t)

	pbData := copyPBData(t)

	report, err := importer.Import(imported, pbData, importer.Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("\n" + report.String())

	if report.HasManualChanges() {
		t.Fatalf("Expected no manual changes, got\n%s", report.String())
	}
	var logs int
	if err := imported.AuxDB().Select("count(*)").From("_logs").Row(&logs); err != nil {
		t.Fatal(err)
	}
	if logs != report.Logs {
		t.Fatalf("Expected %d imported logs, got %d", report.Logs, logs)
	}

	if report.Records["demo1"] == 0 || report.Logs == 0 || report.Params == 0 {
		t.Fatalf("Expected imported records, logs and params, got\n%s", report.String())
	}
	if !strings.Contains(report.String(), "enforced only by the API") {
		t.Fatalf("Expected the report to list the API only rules, got\n%s", report.String())
	}

	reference, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Cleanup()

	compareAPIs(t, imported, reference, report.FailedViews)

	// a repeated import (with force) gives the same result
	report2, err := importer.Import(imported, pbData, importer.Options{Force: true})
	if err != nil {
		t.Fatalf("Expected the repeated import to succeed, got %v", err)
	}
	if !reflect.DeepEqual(report.Records, report2.Records) {
		t.Fatalf("Expected the same imported records, got %v and %v", report.Records, report2.Records)
	}

	compareAPIs(t, imported, reference, report.FailedViews)

	// without force the import refuses a database with collections
	if _, err := importer.Import(imported, pbData, importer.Options{}); err == nil {
		t.Fatal("Expected an error for a database with existing collections")
	}
}

// TestImportManualChanges imports a copy of the test data with a SQLite only
// index and a rule that fails in Postgres: the import succeeds and reports them.
func TestImportManualChanges(t *testing.T) {
	pbData := copyPBData(t)

	src, err := dbx.Open("sqlite", filepath.Join(pbData, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.NewQuery(`
		UPDATE _collections SET
			indexes = json_insert(indexes, '$[#]', 'CREATE INDEX idx_sqlite_only ON demo2 (json_extract(title, ''$.a''))'),
			listRule = 'title > created'
		WHERE name = 'demo2'
	`).Execute()
	src.Close()
	if err != nil {
		t.Fatal(err)
	}

	app := newEmptyApp(t)

	report, err := importer.Import(app, pbData, importer.Options{})
	if err != nil {
		t.Fatalf("Expected the import to succeed, got %v", err)
	}

	t.Log("\n" + report.String())

	if !report.HasManualChanges() {
		t.Fatalf("Expected manual changes, got\n%s", report.String())
	}
	if len(report.FailedIndexes["demo2"]) != 1 || !strings.Contains(report.FailedIndexes["demo2"][0], "idx_sqlite_only") {
		t.Fatalf("Expected the demo2 failed index, got %v", report.FailedIndexes)
	}
	if len(report.FailedRules["demo2"]) != 1 || !strings.HasPrefix(report.FailedRules["demo2"][0], "listRule") {
		t.Fatalf("Expected the demo2 failed list rule, got %v", report.FailedRules)
	}

	// the other indexes and the records are imported
	collection, err := app.FindCollectionByNameOrId("demo2")
	if err != nil {
		t.Fatal(err)
	}
	if collection.GetIndex("idx_sqlite_only") != "" {
		t.Fatal("Expected the failed index to be removed from the collection")
	}
	if report.Records["demo2"] == 0 {
		t.Fatal("Expected the demo2 records to be imported")
	}
}

func TestCopyStorage(t *testing.T) {
	target := t.TempDir()

	if err := importer.CopyStorage(testsDataDir(), target); err != nil {
		t.Fatal(err)
	}

	count := func(root string) int {
		total := 0
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				total++
			}
			return nil
		})
		return total
	}

	expected := count(filepath.Join(testsDataDir(), core.LocalStorageDirName))
	got := count(filepath.Join(target, core.LocalStorageDirName))

	if expected == 0 || got != expected {
		t.Fatalf("Expected %d copied files, got %d", expected, got)
	}

	// missing storage
	if err := importer.CopyStorage(t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("Expected no error for a missing storage directory, got %v", err)
	}
}

func TestReportString(t *testing.T) {
	report := &importer.Report{
		Collections:   []string{"posts", "view1"},
		Records:       map[string]int{"posts": 3},
		FailedViews:   map[string]error{"view1": errors.New("syntax error")},
		FailedIndexes: map[string][]string{"posts": {"CREATE INDEX idx ON posts (x COLLATE NOCASE) (invalid)"}},
		FailedRules:   map[string][]string{"posts": {"listRule: invalid input syntax"}},
		APIOnlyRules:  map[string][]string{"posts": {"createRule: @request.body.a = 1"}},
		Storage:       "local storage",
	}

	str := report.String()

	for _, expected := range []string{
		"posts                    3 records",
		"view1                    view",
		"files: local storage",
		"view1: syntax error",
		"indexes that could not be created",
		"posts: listRule: invalid input syntax",
		"posts: createRule: @request.body.a = 1",
	} {
		if !strings.Contains(str, expected) {
			t.Errorf("Missing %q in the report:\n%s", expected, str)
		}
	}

	if !report.HasManualChanges() {
		t.Fatal("Expected manual changes")
	}

	if (&importer.Report{APIOnlyRules: report.APIOnlyRules}).HasManualChanges() {
		t.Fatal("Expected the API only rules not to require manual changes")
	}
}

func TestFailedIndexesError(t *testing.T) {
	// the shape of the ImportCollections error for an index that can't be created
	err := fmt.Errorf("failed to save collection %q: %w", "posts", validation.Errors{
		"indexes": validation.Errors{"1": errors.New("invalid index")},
	})

	name, failed := importer.ExportFailedIndexes(err)
	if name != "posts" || len(failed) != 1 || failed[1] != "invalid index" {
		t.Fatalf("Unexpected result %q %v", name, failed)
	}

	if name, _ := importer.ExportFailedIndexes(errors.New("other error")); name != "" {
		t.Fatalf("Expected no name for an unrelated error, got %q", name)
	}
}
