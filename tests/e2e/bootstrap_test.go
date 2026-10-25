package e2e

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Robernetes/popgres/apis"
	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
)

// apiClient executes requests against the app router without a real server.
type apiClient struct {
	t     *testing.T
	mux   http.Handler
	token string
}

func newAPIClient(t *testing.T, app *tests.TestApp) *apiClient {
	baseRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}

	serveEvent := new(core.ServeEvent)
	serveEvent.App = app
	serveEvent.Router = baseRouter

	client := &apiClient{t: t}

	err = app.OnServe().Trigger(serveEvent, func(e *core.ServeEvent) error {
		mux, err := e.Router.BuildMux()
		if err != nil {
			return err
		}
		client.mux = mux
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return client
}

func (c *apiClient) do(method string, url string, body any) (int, []byte) {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, url, reader)
	req.Header.Set("content-type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", c.token)
	}

	rec := httptest.NewRecorder()
	c.mux.ServeHTTP(rec, req)

	return rec.Code, rec.Body.Bytes()
}

func (c *apiClient) mustDo(expectedStatus int, method string, url string, body any) map[string]any {
	c.t.Helper()

	status, raw := c.do(method, url, body)
	if status != expectedStatus {
		c.t.Fatalf("%s %s: expected status %d, got %d\n%s", method, url, expectedStatus, status, raw)
	}

	result := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			c.t.Fatalf("%s %s: invalid json response %v\n%s", method, url, err, raw)
		}
	}

	return result
}

// canonicalJSON returns the RFC 8785 form of the provided json
// (the API doesn't guarantee the order of the record keys).
func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()

	v := jsontext.Value(bytes.Clone(raw))
	if err := v.Canonicalize(); err != nil {
		t.Fatalf("invalid json %v\n%s", err, raw)
	}

	return string(v)
}

func TestBootstrapOnEmptyDatabase(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	// system tables and migrations
	// -----------------------------------------------------------
	for _, table := range []string{"_collections", "_params", "_migrations", core.CollectionNameSuperusers, "users", "_mfas", "_otps", "_externalAuths", "_authOrigins"} {
		if !app.HasTable(table) {
			t.Fatalf("Expected table %q to exist", table)
		}
	}
	if !app.AuxHasTable("_logs") {
		t.Fatal("Expected the _logs table to exist in the aux schema")
	}

	var logsSchema string
	err = app.DB().NewQuery("SELECT [[table_schema]] FROM {{information_schema.tables}} WHERE [[table_name]] = '_logs'").Row(&logsSchema)
	if err != nil || logsSchema != core.AuxSchema {
		t.Fatalf("Expected _logs in the %q schema, got %q (%v)", core.AuxSchema, logsSchema, err)
	}

	var totalMigrations int
	if err := app.DB().Select("count(*)").From("_migrations").Row(&totalMigrations); err != nil {
		t.Fatal(err)
	}
	if totalMigrations == 0 {
		t.Fatal("Expected applied migrations")
	}

	// superuser
	// -----------------------------------------------------------
	superusers, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	superuser := core.NewRecord(superusers)
	superuser.SetEmail("admin@example.com")
	superuser.SetPassword("1234567890")
	if err := app.Save(superuser); err != nil {
		t.Fatal(err)
	}

	client := newAPIClient(t, app)

	auth := client.mustDo(http.StatusOK, http.MethodPost, "/api/collections/_superusers/auth-with-password", map[string]any{
		"identity": "admin@example.com",
		"password": "1234567890",
	})
	token, _ := auth["token"].(string)
	if token == "" {
		t.Fatalf("Expected auth token, got %v", auth)
	}
	client.token = token

	// collection with a field of each type
	// -----------------------------------------------------------
	usersCollection, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}

	client.mustDo(http.StatusOK, http.MethodPost, "/api/collections", map[string]any{
		"name":       "posts",
		"type":       "base",
		"listRule":   "",
		"viewRule":   "",
		"createRule": "",
		"updateRule": "",
		"fields": []map[string]any{
			{"name": "title", "type": "text", "required": true},
			{"name": "slug", "type": "text"},
			{"name": "body", "type": "editor"},
			{"name": "views", "type": "number"},
			{"name": "active", "type": "bool"},
			{"name": "contact", "type": "email"},
			{"name": "site", "type": "url"},
			{"name": "published", "type": "date"},
			{"name": "meta", "type": "json"},
			{"name": "status", "type": "select", "maxSelect": 1, "values": []string{"draft", "public"}},
			{"name": "tags", "type": "select", "maxSelect": 3, "values": []string{"a", "b", "c"}},
			{"name": "author", "type": "relation", "collectionId": usersCollection.Id, "maxSelect": 1},
			{"name": "readers", "type": "relation", "collectionId": usersCollection.Id, "maxSelect": 5},
			{"name": "cover", "type": "file", "maxSelect": 1, "maxSize": 1024},
			{"name": "gallery", "type": "file", "maxSelect": 5, "maxSize": 1024},
			{"name": "loc", "type": "geoPoint"},
			{"name": "created", "type": "autodate", "onCreate": true},
			{"name": "updated", "type": "autodate", "onCreate": true, "onUpdate": true},
		},
		"indexes": []string{
			"CREATE UNIQUE INDEX `idx_posts_slug` ON `posts` (`slug` COLLATE NOCASE) WHERE `slug` != ''",
			"CREATE INDEX `idx_posts_published` ON `posts` (`published`)",
		},
	})

	// native column types
	columnTypes := map[string]string{}
	info, err := app.TableInfo("posts")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range info {
		columnTypes[col.Name] = col.Type
	}
	expectedTypes := map[string]string{
		"id":        "text",
		"title":     "text",
		"views":     "double precision",
		"active":    "boolean",
		"published": "timestamp with time zone",
		"created":   "timestamp with time zone",
		"meta":      "jsonb",
		"status":    "text",
		"tags":      "jsonb",
		"author":    "text",
		"readers":   "jsonb",
		"cover":     "text",
		"gallery":   "jsonb",
		"loc":       "jsonb",
	}
	for name, typ := range expectedTypes {
		if columnTypes[name] != typ {
			t.Errorf("Expected column %q to be %q, got %q", name, typ, columnTypes[name])
		}
	}

	indexes, err := app.TableIndexes("posts")
	if err != nil {
		t.Fatal(err)
	}
	if def := indexes["idx_posts_slug"]; !strings.Contains(def, "lower(slug)") || !strings.Contains(def, "WHERE (slug <> ''::text)") {
		t.Fatalf("Unexpected idx_posts_slug definition %q", def)
	}

	// records
	// -----------------------------------------------------------
	reader := core.NewRecord(usersCollection)
	reader.SetEmail("reader@example.com")
	reader.SetPassword("1234567890")
	if err := app.Save(reader); err != nil {
		t.Fatal(err)
	}

	created := client.mustDo(http.StatusOK, http.MethodPost, "/api/collections/posts/records", map[string]any{
		"title":     "Hello",
		"slug":      "Hello-World",
		"body":      "<p>hi</p>",
		"views":     12.5,
		"active":    true,
		"contact":   "a@example.com",
		"site":      "https://example.com",
		"published": "2024-01-02 03:04:05.678Z",
		"meta":      map[string]any{"b": 2, "a": []any{1, "x", nil, true}},
		"status":    "public",
		"tags":      []string{"a", "c"},
		"author":    reader.Id,
		"readers":   []string{reader.Id},
		"loc":       map[string]any{"lon": -79.5, "lat": 8.98},
	})
	id, _ := created["id"].(string)
	createdAt, _ := created["created"].(string)
	updatedAt, _ := created["updated"].(string)
	if len(id) != 15 || createdAt == "" || updatedAt == "" {
		t.Fatalf("Unexpected created record %v", created)
	}

	expectedRecord := `{` +
		`"active":true,` +
		`"author":"` + reader.Id + `",` +
		`"body":"<p>hi</p>",` +
		`"collectionId":"` + created["collectionId"].(string) + `",` +
		`"collectionName":"posts",` +
		`"contact":"a@example.com",` +
		`"cover":"",` +
		`"created":"` + createdAt + `",` +
		`"gallery":[],` +
		`"id":"` + id + `",` +
		`"loc":{"lat":8.98,"lon":-79.5},` +
		`"meta":{"a":[1,"x",null,true],"b":2},` +
		`"published":"2024-01-02 03:04:05.678Z",` +
		`"readers":["` + reader.Id + `"],` +
		`"site":"https://example.com",` +
		`"slug":"Hello-World",` +
		`"status":"public",` +
		`"tags":["a","c"],` +
		`"title":"Hello",` +
		`"updated":"` + updatedAt + `",` +
		`"views":12.5` +
		`}`

	status, raw := client.do(http.MethodGet, "/api/collections/posts/records/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("Expected 200, got %d\n%s", status, raw)
	}
	if got := canonicalJSON(t, raw); got != expectedRecord {
		t.Fatalf("View record mismatch\nexpected: %s\ngot:      %s", expectedRecord, got)
	}

	// empty record (zero values)
	empty := client.mustDo(http.StatusOK, http.MethodPost, "/api/collections/posts/records", map[string]any{
		"title": "Empty",
	})
	emptyId := empty["id"].(string)

	status, raw = client.do(http.MethodGet, "/api/collections/posts/records/"+emptyId+"?fields=published,meta,views,active,tags,readers,cover,gallery,loc,status", nil)
	if status != http.StatusOK {
		t.Fatalf("Expected 200, got %d\n%s", status, raw)
	}
	expectedEmpty := `{"active":false,"cover":"","gallery":[],"loc":{"lat":0,"lon":0},"meta":null,"published":"","readers":[],"status":"","tags":[],"views":0}`
	if got := canonicalJSON(t, raw); got != expectedEmpty {
		t.Fatalf("Empty record mismatch\nexpected: %s\ngot:      %s", expectedEmpty, got)
	}

	// the empty date and json are stored as NULL
	var publishedIsNull, metaIsNull bool
	err = app.DB().NewQuery("SELECT [[published]] IS NULL, [[meta]] IS NULL FROM {{posts}} WHERE [[id]] = {:id}").
		Bind(dbx.Params{"id": emptyId}).
		Row(&publishedIsNull, &metaIsNull)
	if err != nil || !publishedIsNull || !metaIsNull {
		t.Fatalf("Expected NULL published and meta, got %v %v (%v)", publishedIsNull, metaIsNull, err)
	}

	// list (default panel sort)
	list := client.mustDo(http.StatusOK, http.MethodGet, "/api/collections/posts/records?sort=-created,-id&filter=(active=true%20%26%26%20views>10%20%26%26%20title~'hel')", nil)
	if total, _ := list["totalItems"].(float64); total != 1 {
		t.Fatalf("Expected 1 filtered item, got %v", list)
	}

	list = client.mustDo(http.StatusOK, http.MethodGet, "/api/collections/posts/records?sort=-created,-id", nil)
	if total, _ := list["totalItems"].(float64); total != 2 {
		t.Fatalf("Expected 2 items, got %v", list)
	}

	// the SQLite rowid is not supported
	client.mustDo(http.StatusBadRequest, http.MethodGet, "/api/collections/posts/records?sort=-@rowid", nil)

	// update (clear the date and change a number)
	updated := client.mustDo(http.StatusOK, http.MethodPatch, "/api/collections/posts/records/"+id, map[string]any{
		"published": "",
		"views":     13,
	})
	if updated["published"] != "" || updated["views"] != float64(13) || updated["updated"] == updatedAt {
		t.Fatalf("Unexpected updated record %v", updated)
	}

	// unique violation (case-insensitive index)
	result := client.mustDo(http.StatusBadRequest, http.MethodPost, "/api/collections/posts/records", map[string]any{
		"title": "Duplicate",
		"slug":  "hello-world",
	})
	if !strings.Contains(toJSON(t, result), `"slug":{"code":"validation_not_unique"`) {
		t.Fatalf("Expected slug validation_not_unique error, got %v", result)
	}

	// delete
	client.mustDo(http.StatusNoContent, http.MethodDelete, "/api/collections/posts/records/"+emptyId, nil)
	err = app.DB().Select("id").From("posts").Where(dbx.HashExp{"id": emptyId}).Row(new(string))
	if err != sql.ErrNoRows {
		t.Fatalf("Expected the record to be deleted, got %v", err)
	}

	// migrations are idempotent
	// -----------------------------------------------------------
	if err := app.RunAllMigrations(); err != nil {
		t.Fatalf("Failed to rerun the migrations: %v", err)
	}
	var totalAfter int
	if err := app.DB().Select("count(*)").From("_migrations").Row(&totalAfter); err != nil {
		t.Fatal(err)
	}
	if totalAfter != totalMigrations {
		t.Fatalf("Expected %d applied migrations after rerun, got %d", totalMigrations, totalAfter)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}
