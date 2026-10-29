package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Robernetes/popgres/apis"
	"github.com/Robernetes/popgres/apis/mcp"
	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// authTransport adds the Authorization header to the MCP client requests.
type authTransport struct {
	token string
}

func (t *authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func startMCPServer(t *testing.T, app *tests.TestApp, enabled bool) *httptest.Server {
	t.Helper()

	app.Settings().MCP.Enabled = enabled
	if err := app.Save(app.Settings()); err != nil {
		t.Fatal(err)
	}

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

func connectMCP(t *testing.T, baseURL string, token string) *sdk.ClientSession {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	client := sdk.NewClient(&sdk.Implementation{Name: "popgres-test", Version: "1"}, nil)

	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   baseURL + apis.MCPPath,
		HTTPClient: &http.Client{Transport: &authTransport{token: token}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	return session
}

func toolNames(t *testing.T, session *sdk.ClientSession) []string {
	t.Helper()

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, len(result.Tools))
	for i, tool := range result.Tools {
		names[i] = tool.Name
	}

	return names
}

func callTool(t *testing.T, session *sdk.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}

	text, _ := result.Content[0].(*sdk.TextContent)
	data := map[string]any{}
	if text != nil {
		_ = json.Unmarshal([]byte(text.Text), &data)
	}

	return data, result.IsError
}

func TestMCPDisabled(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	server := startMCPServer(t, app, false)

	superuser, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	token, err := superuser.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, server.URL+apis.MCPPath, strings.NewReader(`{}`))
		req.Header.Set("Authorization", token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()

		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("[%s] Expected 404 with the disabled MCP server, got %d", method, res.StatusCode)
		}
	}
}

func TestMCPUnauthorized(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	server := startMCPServer(t, app, true)

	res, err := http.Post(server.URL+apis.MCPPath, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Expected 401 without an auth token, got %d", res.StatusCode)
	}
}

func TestMCPTools(t *testing.T) {
	app, user1, user2 := newVectorFixture(t)

	ownerRule := "owner = @request.auth.id"
	posts := core.NewBaseCollection("mposts")
	posts.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "owner"})
	posts.ListRule = &ownerRule
	posts.ViewRule = &ownerRule
	posts.CreateRule = &ownerRule
	if err := app.Save(posts); err != nil {
		t.Fatal(err)
	}
	for i, owner := range []*core.Record{user1, user1, user2} {
		record := core.NewRecord(posts)
		record.Set("title", "post"+string(rune('a'+i)))
		record.Set("owner", owner.Id)
		if err := app.Save(record); err != nil {
			t.Fatal(err)
		}
	}

	server := startMCPServer(t, app, true)

	token1, err := user1.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	session := connectMCP(t, server.URL, token1)

	// the tools follow the collection rules
	names := toolNames(t, session)
	for _, expected := range []string{"list_mposts", "get_mposts", "create_mposts", "list_vdocs", "vector_search_vdocs"} {
		if !slices.Contains(names, expected) {
			t.Fatalf("Expected tool %q in %v", expected, names)
		}
	}
	for _, unexpected := range []string{"update_mposts", "delete_mposts", "list_demo1", "list__superusers"} {
		if slices.Contains(names, unexpected) {
			t.Fatalf("Didn't expect tool %q in %v", unexpected, names)
		}
	}

	// list respects the list rule
	result, isError := callTool(t, session, "list_mposts", map[string]any{"sort": "title"})
	if isError {
		t.Fatalf("Unexpected list error %v", result)
	}
	if total, _ := result["totalItems"].(float64); total != 2 {
		t.Fatalf("Expected 2 own posts, got %v", result["totalItems"])
	}

	// create follows the create rule
	result, isError = callTool(t, session, "create_mposts", map[string]any{"data": map[string]any{"title": "new", "owner": user1.Id}})
	if isError || result["title"] != "new" {
		t.Fatalf("Expected the record to be created, got %v (error %v)", result, isError)
	}
	_, isError = callTool(t, session, "create_mposts", map[string]any{"data": map[string]any{"title": "other", "owner": user2.Id}})
	if !isError {
		t.Fatal("Expected the create rule to reject a record of another owner")
	}

	// get with the created id
	created, _ := result["id"].(string)
	result, isError = callTool(t, session, "get_mposts", map[string]any{"id": created, "fields": "id,title"})
	if isError || result["title"] != "new" || result["owner"] != nil {
		t.Fatalf("Unexpected get result %v (error %v)", result, isError)
	}

	// vector search
	result, isError = callTool(t, session, "vector_search_vdocs", map[string]any{"field": "embedding", "vector": []float64{1, 0, 0}, "k": 2})
	if isError {
		t.Fatalf("Unexpected vector search error %v", result)
	}
	if items, _ := result["items"].([]any); len(items) != 2 {
		t.Fatalf("Expected 2 vector search items, got %v", result)
	}

	// schema resource
	resource, err := session.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: mcp.SchemaResourceURI})
	if err != nil {
		t.Fatal(err)
	}
	if text := resource.Contents[0].Text; !strings.Contains(text, `"name":"mposts"`) ||
		strings.Contains(text, `"name":"_superusers"`) ||
		strings.Contains(text, `"name":"demo1"`) {
		t.Fatalf("Unexpected schema resource %s", text)
	}

	// the superusers have the tools of all collections
	superuser, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	superuserToken, err := superuser.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	superuserSession := connectMCP(t, server.URL, superuserToken)
	names = toolNames(t, superuserSession)
	for _, expected := range []string{"list_demo1", "update_mposts", "delete_mposts"} {
		if !slices.Contains(names, expected) {
			t.Fatalf("Expected superuser tool %q in %v", expected, names)
		}
	}

	// and the schema of the superusers only collections
	resource, err = superuserSession.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: mcp.SchemaResourceURI})
	if err != nil {
		t.Fatal(err)
	}
	if text := resource.Contents[0].Text; !strings.Contains(text, `"name":"demo1"`) {
		t.Fatalf("Expected the superusers only collection in the superuser schema resource %s", text)
	}
}
