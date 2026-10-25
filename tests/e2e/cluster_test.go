package e2e

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robernetes/popgres/apis"
	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
)

// clusterNode is an app instance with its own HTTP server.
type clusterNode struct {
	app *tests.TestApp
	srv *httptest.Server
}

// newTestCluster starts the specified number of app instances (nodes)
// in cluster mode that share the same test database (with the test fixtures).
func newTestCluster(t *testing.T, total int) []*clusterNode {
	t.Helper()

	testDB, err := tests.NewFixturesTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tests.DropTestDatabase(testDB.Name) })

	nodes := make([]*clusterNode, total)
	for i := range nodes {
		app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
			DatabaseURL: testDB.URL,
			ClusterMode: true,
		})
		if err != nil {
			t.Fatal(err)
		}

		nodes[i] = newClusterNode(t, app)
	}

	return nodes
}

// newClusterNode starts an HTTP server for the test app (closed on the test cleanup).
func newClusterNode(t *testing.T, app *tests.TestApp) *clusterNode {
	t.Helper()

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		app.Cleanup()
		t.Fatal(err)
	}

	mux, err := pbRouter.BuildMux()
	if err != nil {
		app.Cleanup()
		t.Fatal(err)
	}

	srv := httptest.NewServer(mux)

	// note: registered in reverse order so that the server is closed before the app cleanup
	t.Cleanup(app.Cleanup)
	t.Cleanup(srv.Close)

	select {
	case <-app.Cluster().Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("cluster LISTEN timeout")
	}

	return &clusterNode{app: app, srv: srv}
}

func superuserToken(t *testing.T, app core.App) string {
	t.Helper()

	superuser, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}

	token, err := superuser.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	return token
}

func doRequest(t *testing.T, method string, url string, body string, token string) (int, string) {
	t.Helper()

	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)

	return res.StatusCode, string(raw)
}

// sseClient is a minimal realtime (SSE) client, similar to the one of the JS SDK.
type sseClient struct {
	clientId string
	events   chan sseEvent
	close    func()
}

type sseEvent struct {
	name string
	data string
}

func connectRealtime(t *testing.T, baseURL string) *sseClient {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	client := &sseClient{
		events: make(chan sseEvent, 100),
		close:  func() { res.Body.Close() },
	}
	t.Cleanup(client.close)

	go func() {
		scanner := bufio.NewScanner(res.Body)
		event := sseEvent{}
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				event.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				event.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			case line == "":
				if event.name != "" {
					client.events <- event
				}
				event = sseEvent{}
			}
		}
	}()

	connect := client.next(t, 5*time.Second)
	if connect == nil || connect.name != "PB_CONNECT" {
		t.Fatalf("Expected PB_CONNECT event, got %v", connect)
	}

	data := struct {
		ClientId string `json:"clientId"`
	}{}
	if err := json.Unmarshal([]byte(connect.data), &data); err != nil {
		t.Fatal(err)
	}
	client.clientId = data.ClientId

	return client
}

func (c *sseClient) next(t *testing.T, timeout time.Duration) *sseEvent {
	t.Helper()

	select {
	case e := <-c.events:
		return &e
	case <-time.After(timeout):
		return nil
	}
}

func (c *sseClient) expectRecordEvent(t *testing.T, action string, recordId string) {
	t.Helper()

	e := c.next(t, 5*time.Second)
	if e == nil {
		t.Fatalf("Expected %s event for record %s, got none", action, recordId)
	}

	data := struct {
		Action string         `json:"action"`
		Record map[string]any `json:"record"`
	}{}
	if err := json.Unmarshal([]byte(e.data), &data); err != nil {
		t.Fatal(err)
	}

	if data.Action != action || data.Record["id"] != recordId {
		t.Fatalf("Expected %s event for record %s, got %s %s", action, recordId, e.name, e.data)
	}
}

func (c *sseClient) expectNoEvent(t *testing.T) {
	t.Helper()

	if e := c.next(t, 700*time.Millisecond); e != nil {
		t.Fatalf("Expected no event, got %s %s", e.name, e.data)
	}
}

func TestClusterRealtime(t *testing.T) {
	t.Parallel()

	nodes := newTestCluster(t, 3)
	nodeA, nodeB, nodeC := nodes[0], nodes[1], nodes[2]

	token := superuserToken(t, nodeC.app)

	// a guest client connected to node A that subscribes through node B
	// (the request is forwarded to node A)
	allowed := connectRealtime(t, nodeA.srv.URL)
	if !strings.HasPrefix(allowed.clientId, nodeA.app.Cluster().NodeId()+"_") {
		t.Fatalf("Expected the client id to start with the node A id, got %q", allowed.clientId)
	}

	status, body := doRequest(t, http.MethodPost, nodeB.srv.URL+"/api/realtime",
		`{"clientId":"`+allowed.clientId+`","subscriptions":["demo2/*"]}`, "")
	if status != http.StatusNoContent {
		t.Fatalf("Expected the forwarded subscriptions request to succeed, got %d: %s", status, body)
	}

	// a guest client connected to node A without access to demo3
	denied := connectRealtime(t, nodeA.srv.URL)
	status, body = doRequest(t, http.MethodPost, nodeA.srv.URL+"/api/realtime",
		`{"clientId":"`+denied.clientId+`","subscriptions":["demo3/*"]}`, "")
	if status != http.StatusNoContent {
		t.Fatalf("Expected the subscriptions request to succeed, got %d: %s", status, body)
	}

	// create, update and delete on node C
	status, body = doRequest(t, http.MethodPost, nodeC.srv.URL+"/api/collections/demo2/records", `{"title":"cluster"}`, "")
	if status != http.StatusOK {
		t.Fatalf("Failed to create record: %d %s", status, body)
	}
	record := map[string]any{}
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		t.Fatal(err)
	}
	recordId, _ := record["id"].(string)
	allowed.expectRecordEvent(t, "create", recordId)

	status, body = doRequest(t, http.MethodPatch, nodeC.srv.URL+"/api/collections/demo2/records/"+recordId, `{"title":"cluster_updated"}`, "")
	if status != http.StatusOK {
		t.Fatalf("Failed to update record: %d %s", status, body)
	}
	allowed.expectRecordEvent(t, "update", recordId)

	status, body = doRequest(t, http.MethodDelete, nodeC.srv.URL+"/api/collections/demo2/records/"+recordId, "", "")
	if status != http.StatusNoContent {
		t.Fatalf("Failed to delete record: %d %s", status, body)
	}
	allowed.expectRecordEvent(t, "delete", recordId)

	// a change without access
	status, body = doRequest(t, http.MethodPost, nodeC.srv.URL+"/api/collections/demo3/records", `{"title":"cluster"}`, token)
	if status != http.StatusOK {
		t.Fatalf("Failed to create demo3 record: %d %s", status, body)
	}
	denied.expectNoEvent(t)
	allowed.expectNoEvent(t)
}

func TestClusterCollectionsSync(t *testing.T) {
	t.Parallel()

	nodes := newTestCluster(t, 3)

	collection := core.NewBaseCollection("cluster_sync")
	if err := nodes[0].app.Save(collection); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	for _, node := range nodes[1:] {
		for {
			if _, err := node.app.FindCachedCollectionByNameOrId("cluster_sync"); err == nil {
				break
			}
			if time.Since(start) > time.Second {
				t.Fatal("Expected the new collection to be visible on the other nodes in less than 1s")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestClusterCron(t *testing.T) {
	t.Parallel()

	nodes := newTestCluster(t, 3)

	const interval = 400 * time.Millisecond

	var mu sync.Mutex
	runs := map[string]int{}

	for _, node := range nodes {
		node.app.Cron().SetInterval(interval)
		node.app.Cron().MustAdd("cluster_test", "* * * * *", func() {
			mu.Lock()
			runs[node.app.Cluster().NodeId()]++
			mu.Unlock()
		})
	}
	for _, node := range nodes {
		node.app.Cron().Start()
	}

	// at least 5 windows
	time.Sleep(6*interval + 100*time.Millisecond)

	for _, node := range nodes {
		node.app.Cron().Stop()
	}
	time.Sleep(300 * time.Millisecond)

	var slots []time.Time
	err := nodes[0].app.DB().Select("slot").
		From("_cron_runs").
		Where(dbx.HashExp{"job_id": "cluster_test"}).
		Column(&slots)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	total := 0
	for _, n := range runs {
		total += n
	}

	if len(slots) < 5 {
		t.Fatalf("Expected at least 5 windows, got %d", len(slots))
	}

	if total != len(slots) {
		t.Fatalf("Expected exactly one run per window (%d windows), got %d runs: %v", len(slots), total, runs)
	}
}

func TestClusterRateLimit(t *testing.T) {
	t.Parallel()

	nodes := newTestCluster(t, 3)

	// the settings are saved on one node and reloaded by the others
	settings := nodes[0].app.Settings()
	settings.RateLimits.Enabled = true
	settings.RateLimits.Rules = []core.RateLimitRule{
		{Label: "/api/health", MaxRequests: 4, Duration: 60},
	}
	if err := nodes[0].app.Save(settings); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for _, node := range nodes[1:] {
		for !node.app.Settings().RateLimits.Enabled {
			if time.Now().After(deadline) {
				t.Fatal("Expected the rate limit settings to be reloaded by the other nodes")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// round robin
	for i := 0; i < 6; i++ {
		node := nodes[i%len(nodes)]

		status, body := doRequest(t, http.MethodGet, node.srv.URL+"/api/health", "", "")

		expected := http.StatusOK
		if i >= 4 {
			expected = http.StatusTooManyRequests
		}

		if status != expected {
			t.Fatalf("[%d] Expected status %d, got %d: %s", i, expected, status, body)
		}
	}
}

func TestClusterConcurrentBootstrap(t *testing.T) {
	t.Parallel()

	testDB, err := tests.NewTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer tests.DropTestDatabase(testDB.Name)

	const total = 3

	apps := make([]*core.BaseApp, total)
	for i := range apps {
		apps[i] = core.NewBaseApp(core.BaseAppConfig{
			DataDir:     t.TempDir(),
			DatabaseURL: testDB.URL,
			ClusterMode: true,
		})
	}

	errs := make(chan error, total)
	for _, app := range apps {
		go func() { errs <- app.Bootstrap() }()
	}
	for range total {
		if err := <-errs; err != nil {
			t.Fatalf("Failed to bootstrap: %v", err)
		}
	}
	defer func() {
		for _, app := range apps {
			_ = app.ClearBootstrap()
		}
	}()

	var applied []string
	if err := apps[0].DB().Select("file").From(core.DefaultMigrationsTable).Column(&applied); err != nil {
		t.Fatal(err)
	}

	expected := len(core.SystemMigrations.Items())
	if len(applied) != expected {
		t.Fatalf("Expected %d applied migrations, got %d: %v", expected, len(applied), applied)
	}

	seen := map[string]bool{}
	for _, file := range applied {
		if seen[file] {
			t.Fatalf("Duplicated applied migration %q", file)
		}
		seen[file] = true
	}
}
