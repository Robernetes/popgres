package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
)

// TestFullTextSearch checks the search field through the API (filter, sort,
// export and API rule) and through the direct SQL clients (RLS privileges).
func TestFullTextSearch(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	// the drafts are not listed (a rule with the search field, also compiled to RLS)
	listRule := "content !~ 'draft'"
	createRule := ""

	collection := core.NewBaseCollection("articles")
	collection.Fields.Add(
		&core.TextField{Name: "title"},
		&core.EditorField{Name: "body"},
		&core.SearchField{Name: "content", Fields: []string{"title", "body"}, Language: "english"},
	)
	collection.ListRule = &listRule
	collection.CreateRule = &createRule
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	client := newAPIClient(t, app)

	for _, data := range []map[string]any{
		{"title": "Running shoes", "body": "<p>For the marathon</p>"},
		{"title": "Shoe polish", "body": "<p>Keeps the leather clean</p>", "content": "ignored"},
		{"title": "Draft about shoes", "body": "<p>Not ready</p>"},
		{"title": "Weather", "body": "<p>Sunny</p>"},
	} {
		created := client.mustDo(200, http.MethodPost, "/api/collections/articles/records", data)
		if _, ok := created["content"]; ok {
			t.Fatalf("Expected the search field to not be returned, got %v", created)
		}
	}

	list := func(query string) []string {
		t.Helper()

		result := client.mustDo(200, http.MethodGet, "/api/collections/articles/records?"+query, nil)
		items, _ := result["items"].([]any)
		titles := make([]string, len(items))
		for i, item := range items {
			record, _ := item.(map[string]any)
			if _, ok := record["content"]; ok {
				t.Fatalf("Expected the search field to not be returned, got %v", record)
			}
			titles[i], _ = record["title"].(string)
		}
		return titles
	}

	equal := func(name string, got []string, expected ...string) {
		t.Helper()
		if !slices.Equal(got, expected) {
			t.Fatalf("[%s] Expected %v, got %v", name, expected, got)
		}
	}

	// "shoes" matches "shoe" (english stemming), the title is more relevant than the body
	// and the draft is excluded by the list rule
	equal("filter and rank",
		list("filter="+url.QueryEscape("content ~ 'shoes'")+"&sort="+url.QueryEscape("-@textRank(content, 'shoes'),title")),
		"Running shoes", "Shoe polish",
	)
	equal("phrase", list("filter="+url.QueryEscape(`content ~ '"running shoes"'`)+"&sort=title"), "Running shoes")
	equal("negation", list("filter="+url.QueryEscape("content ~ 'shoes -marathon'")+"&sort=title"), "Shoe polish")

	// invalid usage
	for _, query := range []string{
		"filter=" + url.QueryEscape("content = 'shoes'"),
		"filter=" + url.QueryEscape("content ~ title"),
		"sort=" + url.QueryEscape("@textRank(title, 'shoes')"),
	} {
		if status, raw := client.do(http.MethodGet, "/api/collections/articles/records?"+query, nil); status != 400 {
			t.Fatalf("Expected 400 for %s, got %d\n%s", query, status, raw)
		}
	}

	// direct SQL: the search column can be read but not written, and the
	// list rule policy uses the same search expression
	var titles []string
	err = tests.RunWithRLSRole(app, core.RLSAnonRole, "", func(txApp core.App) error {
		_, err := txApp.DB().NewQuery("INSERT INTO {{articles}} ([[id]], [[title]], [[body]]) VALUES ('sqlarticle0001', 'Trail shoes', '')").Execute()
		if err != nil {
			return err
		}

		return txApp.DB().Select("title").
			From("articles").
			Where(dbx.NewExp("[[content]] @@ websearch_to_tsquery('english', {:q})", dbx.Params{"q": "shoes"})).
			OrderBy("title").
			Column(&titles)
	})
	if err != nil {
		t.Fatal(err)
	}
	equal("direct sql", titles, "Running shoes", "Shoe polish", "Trail shoes")

	err = tests.RunWithRLSRole(app, core.RLSAnonRole, "", func(txApp core.App) error {
		_, err := txApp.DB().NewQuery("UPDATE {{articles}} SET [[content]] = to_tsvector('x')").Execute()
		return err
	})
	if err == nil {
		t.Fatal("Expected the search column update to fail")
	}
}
