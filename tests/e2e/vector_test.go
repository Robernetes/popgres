package e2e

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/Robernetes/popgres/tools/types"
	"github.com/pocketbase/dbx"
)

// newVectorFixture creates the "vdocs" collection with a vector field and
// the "owner = @request.auth.id || public = true" list rule.
func newVectorFixture(t *testing.T) (*tests.TestApp, *core.Record, *core.Record) {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	users, err := app.FindAllRecords("users", dbx.NewExp("1=1"))
	if err != nil || len(users) < 2 {
		t.Fatalf("Expected at least 2 users, got %d (%v)", len(users), err)
	}

	rule := "owner = @request.auth.id || public = true"

	collection := core.NewBaseCollection("vdocs")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.BoolField{Name: "public"},
		&core.RelationField{Name: "owner", CollectionId: users[0].Collection().Id, MaxSelect: 1},
		&core.VectorField{Name: "embedding", Dimensions: 3, Index: true},
	)
	collection.ListRule = &rule
	collection.ViewRule = &rule
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	docs := []struct {
		title     string
		public    bool
		owner     *core.Record
		embedding types.Vector
	}{
		{"a", true, users[0], types.Vector{1, 0, 0}},
		{"b", true, users[0], types.Vector{0.9, 0.1, 0}},
		{"c", false, users[1], types.Vector{0.95, 0.05, 0}}, // private of user 2
		{"d", true, users[1], types.Vector{0.5, 0.5, 0}},
		{"e", true, users[1], types.Vector{0, 0, 1}},
		{"f", true, users[0], nil}, // without embedding
	}

	for _, d := range docs {
		record := core.NewRecord(collection)
		record.Set("title", d.title)
		record.Set("public", d.public)
		record.Set("owner", d.owner.Id)
		record.Set("embedding", d.embedding)
		if err := app.Save(record); err != nil {
			t.Fatal(err)
		}
	}

	return app, users[0], users[1]
}

func vectorSearchTitles(t *testing.T, result map[string]any) []string {
	t.Helper()

	items, _ := result["items"].([]any)
	titles := make([]string, len(items))
	for i, item := range items {
		record, _ := item.(map[string]any)
		titles[i], _ = record["title"].(string)
		if _, ok := record["score"].(float64); !ok {
			t.Fatalf("Expected a numeric score, got %v", record["score"])
		}
	}

	return titles
}

func TestVectorSearch(t *testing.T) {
	app, user1, user2 := newVectorFixture(t)

	client := newAPIClient(t, app)

	search := func(body map[string]any) []string {
		return vectorSearchTitles(t, client.mustDo(200, http.MethodPost, "/api/collections/vdocs/vector-search", body))
	}

	equal := func(name string, got []string, expected ...string) {
		t.Helper()
		if len(got) != len(expected) {
			t.Fatalf("[%s] Expected %v, got %v", name, expected, got)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("[%s] Expected %v, got %v", name, expected, got)
			}
		}
	}

	// guest: the private record "c" is excluded by the list rule
	equal("guest", search(map[string]any{"field": "embedding", "vector": []float64{1, 0, 0}, "k": 3}), "a", "b", "d")

	// with a filter
	equal("filter", search(map[string]any{"field": "embedding", "vector": []float64{1, 0, 0}, "k": 2, "filter": "title != 'a'"}), "b", "d")

	// the owner of the private record
	var err error
	client.token, err = user2.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	equal("owner", search(map[string]any{"field": "embedding", "vector": []float64{1, 0, 0}, "k": 3}), "a", "c", "b")

	// another user
	client.token, err = user1.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	equal("other user", search(map[string]any{"field": "embedding", "vector": []float64{1, 0, 0}, "k": 3}), "a", "b", "d")

	// expand and fields
	result := client.mustDo(200, http.MethodPost, "/api/collections/vdocs/vector-search?expand=owner&fields=title,score,expand.owner.id", map[string]any{
		"field":  "embedding",
		"vector": []float64{1, 0, 0},
		"k":      1,
	})
	items, _ := result["items"].([]any)
	first, _ := items[0].(map[string]any)
	if first["title"] != "a" || first["embedding"] != nil || first["score"] == nil {
		t.Fatalf("Unexpected picked fields %v", first)
	}
	expand, _ := first["expand"].(map[string]any)
	owner, _ := expand["owner"].(map[string]any)
	if owner["id"] != user1.Id {
		t.Fatalf("Expected the expanded owner %q, got %v", user1.Id, expand)
	}

	// the cosine score of the exact match
	result = client.mustDo(200, http.MethodPost, "/api/collections/vdocs/vector-search", map[string]any{
		"field":  "embedding",
		"vector": []float64{0, 0, 2},
		"k":      1,
	})
	items, _ = result["items"].([]any)
	first, _ = items[0].(map[string]any)
	if score, _ := first["score"].(float64); score < 0.9999 {
		t.Fatalf("Expected a score of 1 for the exact match, got %v", first["score"])
	}

	// the records list sorted by the vector distance
	list := client.mustDo(200, http.MethodGet, "/api/collections/vdocs/records?perPage=2&sort="+url.QueryEscape("@vectorDistance(embedding,[0,1,0]),title"), nil)
	listItems, _ := list["items"].([]any)
	listFirst, _ := listItems[0].(map[string]any)
	if listFirst["title"] != "d" {
		t.Fatalf("Expected the closest record first in the sorted list, got %v", listFirst["title"])
	}

	// validation errors
	for _, body := range []map[string]any{
		{"field": "title", "vector": []float64{1, 0, 0}},
		{"field": "missing", "vector": []float64{1, 0, 0}},
		{"field": "embedding", "vector": []float64{1, 0}},
		{"field": "embedding"},
		{"field": "embedding", "vector": []float64{1, 0, 0}, "k": 1001},
		{"field": "embedding", "vector": []float64{1, 0, 0}, "filter": "missing = 1"},
	} {
		if status, raw := client.do(http.MethodPost, "/api/collections/vdocs/vector-search", body); status != 400 {
			t.Fatalf("Expected 400 for %v, got %d\n%s", body, status, raw)
		}
	}

	// superusers only collection
	client.token = ""
	if status, raw := client.do(http.MethodPost, "/api/collections/demo1/vector-search", map[string]any{"field": "x", "vector": []float64{1}}); status != 403 {
		t.Fatalf("Expected 403 for a guest and a superusers only collection, got %d\n%s", status, raw)
	}
}
