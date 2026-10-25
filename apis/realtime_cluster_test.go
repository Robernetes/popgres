package apis_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Robernetes/popgres/apis"
	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/Robernetes/popgres/tools/subscriptions"
)

// newClusterTestApps creates the specified number of test app instances
// (nodes) in cluster mode that share the same test database.
func newClusterTestApps(t *testing.T, total int) []*tests.TestApp {
	testDB, err := tests.NewFixturesTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tests.DropTestDatabase(testDB.Name) })

	apps := make([]*tests.TestApp, total)
	for i := range apps {
		app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
			DatabaseURL: testDB.URL,
			ClusterMode: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Cleanup)

		// registers the realtime hooks
		if _, err := apis.NewRouter(app); err != nil {
			t.Fatal(err)
		}

		select {
		case <-app.Cluster().Ready():
		case <-time.After(5 * time.Second):
			t.Fatal("cluster LISTEN timeout")
		}

		apps[i] = app
	}

	return apps
}

type realtimeTestMessage struct {
	Action string         `json:"action"`
	Record map[string]any `json:"record"`
}

func receiveRealtimeMessage(t *testing.T, client subscriptions.Client, timeout time.Duration) (subscriptions.Message, bool) {
	select {
	case msg := <-client.Channel():
		return msg, true
	case <-time.After(timeout):
		return subscriptions.Message{}, false
	}
}

func expectRealtimeMessage(t *testing.T, client subscriptions.Client, action string, recordId string) {
	t.Helper()

	msg, ok := receiveRealtimeMessage(t, client, 5*time.Second)
	if !ok {
		t.Fatalf("Expected %s message for record %s, got none", action, recordId)
	}

	data := realtimeTestMessage{}
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		t.Fatal(err)
	}

	if data.Action != action || data.Record["id"] != recordId {
		t.Fatalf("Expected %s message for record %s, got %s", action, recordId, msg.Data)
	}
}

func expectNoRealtimeMessage(t *testing.T, client subscriptions.Client) {
	t.Helper()

	if msg, ok := receiveRealtimeMessage(t, client, 700*time.Millisecond); ok {
		t.Fatalf("Expected no message, got %s: %s", msg.Name, msg.Data)
	}
}

func TestRealtimeClusterRecordEvents(t *testing.T) {
	t.Parallel()

	apps := newClusterTestApps(t, 2)
	nodeA, nodeB := apps[0], apps[1]

	// guest subscribed to a public collection
	public := subscriptions.NewDefaultClient()
	public.Subscribe("demo2/*")
	nodeA.SubscriptionsBroker().Register(public)

	// guest subscribed to a collection with an auth only list rule
	restricted := subscriptions.NewDefaultClient()
	restricted.Subscribe("demo3/*")
	nodeA.SubscriptionsBroker().Register(restricted)

	// create on node B
	collection, err := nodeB.FindCollectionByNameOrId("demo2")
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(collection)
	record.Set("title", "cluster_test")
	if err := nodeB.Save(record); err != nil {
		t.Fatal(err)
	}
	expectRealtimeMessage(t, public, "create", record.Id)

	// update on node B
	record.Set("title", "cluster_test_updated")
	if err := nodeB.Save(record); err != nil {
		t.Fatal(err)
	}
	expectRealtimeMessage(t, public, "update", record.Id)

	// delete on node B
	if err := nodeB.Delete(record); err != nil {
		t.Fatal(err)
	}
	expectRealtimeMessage(t, public, "delete", record.Id)

	// no access
	restrictedRecord, err := nodeB.FindRecordById("demo3", "mk5fmymtx4wsprk")
	if err != nil {
		t.Fatal(err)
	}
	restrictedRecord.Set("title", "cluster_test")
	if err := nodeB.Save(restrictedRecord); err != nil {
		t.Fatal(err)
	}
	expectNoRealtimeMessage(t, restricted)

	// the events are not broadcasted twice to the clients of the node that changed the record
	localClient := subscriptions.NewDefaultClient()
	localClient.Subscribe("demo2/*")
	nodeB.SubscriptionsBroker().Register(localClient)

	record2 := core.NewRecord(collection)
	record2.Set("title", "cluster_test2")
	if err := nodeB.Save(record2); err != nil {
		t.Fatal(err)
	}
	expectRealtimeMessage(t, localClient, "create", record2.Id)
	expectRealtimeMessage(t, public, "create", record2.Id)
	expectNoRealtimeMessage(t, localClient)
}

func TestRealtimeClusterDeleteSnapshotRule(t *testing.T) {
	t.Parallel()

	apps := newClusterTestApps(t, 2)
	nodeA, nodeB := apps[0], apps[1]

	// the demo5 list rule depends on the record data (select_many:length = 3)
	client := subscriptions.NewDefaultClient()
	client.Subscribe("demo5/*")
	nodeA.SubscriptionsBroker().Register(client)

	collection, err := nodeB.FindCollectionByNameOrId("demo5")
	if err != nil {
		t.Fatal(err)
	}

	newRecord := func(selectMany []string) *core.Record {
		record := core.NewRecord(collection)
		record.Set("select_many", selectMany)
		if err := nodeB.SaveNoValidate(record); err != nil {
			t.Fatal(err)
		}
		return record
	}

	matching := newRecord([]string{"a", "b", "c"})
	expectRealtimeMessage(t, client, "create", matching.Id)

	nonMatching := newRecord([]string{"a"})
	expectNoRealtimeMessage(t, client)

	// the deleted records are checked against their snapshot on the other nodes
	if err := nodeB.Delete(nonMatching); err != nil {
		t.Fatal(err)
	}
	expectNoRealtimeMessage(t, client)

	if err := nodeB.Delete(matching); err != nil {
		t.Fatal(err)
	}
	expectRealtimeMessage(t, client, "delete", matching.Id)
}

func TestRealtimeClusterDisabled(t *testing.T) {
	t.Parallel()

	testDB, err := tests.NewFixturesTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer tests.DropTestDatabase(testDB.Name)

	// without cluster mode the record events are not published to the other nodes
	apps := make([]*tests.TestApp, 2)
	for i := range apps {
		app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{DatabaseURL: testDB.URL})
		if err != nil {
			t.Fatal(err)
		}
		defer app.Cleanup()

		if _, err := apis.NewRouter(app); err != nil {
			t.Fatal(err)
		}

		<-app.Cluster().Ready()

		apps[i] = app
	}

	client := subscriptions.NewDefaultClient()
	client.Subscribe("demo2/*")
	apps[0].SubscriptionsBroker().Register(client)

	collection, err := apps[1].FindCollectionByNameOrId("demo2")
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(collection)
	record.Set("title", "cluster_test")
	if err := apps[1].Save(record); err != nil {
		t.Fatal(err)
	}

	expectNoRealtimeMessage(t, client)
}

func TestRealtimeClusterForwardSubscriptions(t *testing.T) {
	t.Parallel()

	apps := newClusterTestApps(t, 2)
	nodeA, nodeB := apps[0], apps[1]

	routerB, err := apis.NewRouter(nodeB)
	if err != nil {
		t.Fatal(err)
	}
	muxB, err := routerB.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	// a client connected to node A
	client := subscriptions.NewDefaultClientWithIdPrefix(nodeA.Cluster().NodeId() + "_")
	nodeA.SubscriptionsBroker().Register(client)

	user, err := nodeA.FindAuthRecordByEmail("users", "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	token, err := user.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	// the subscriptions request is received by node B
	body := `{"clientId":"` + client.Id() + `","subscriptions":["demo2","demo3/*"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/realtime", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)
	rec := httptest.NewRecorder()

	muxB.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("Expected status 204, got %d: %s", rec.Code, rec.Body.String())
	}

	for _, sub := range []string{"demo2", "demo3/*"} {
		if !client.HasSubscription(sub) {
			t.Fatalf("Expected the node A client to have the %q subscription, got %v", sub, client.Subscriptions())
		}
	}

	clientAuth, _ := client.Get(apis.RealtimeClientAuthKey).(*core.Record)
	if clientAuth == nil || clientAuth.Id != user.Id {
		t.Fatalf("Expected the node A client auth to be set to %q, got %v", user.Id, clientAuth)
	}

	// a forwarded request is processed like a local one (eg. auth change is forbidden)
	user2, err := nodeA.FindAuthRecordByEmail("users", "test2@example.com")
	if err != nil {
		t.Fatal(err)
	}
	token2, err := user2.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/realtime", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token2)
	rec = httptest.NewRecorder()

	muxB.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("Expected status 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// a client of a missing node
	body = `{"clientId":"nmissingnode_abcdefghijklmnopqrstuvwxyz0","subscriptions":["demo2"]}`
	req = httptest.NewRequest(http.MethodPost, "/api/realtime", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()

	muxB.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("Expected status 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRealtimeClusterForwardOAuth2Redirect(t *testing.T) {
	t.Parallel()

	apps := newClusterTestApps(t, 2)
	nodeA, nodeB := apps[0], apps[1]

	routerB, err := apis.NewRouter(nodeB)
	if err != nil {
		t.Fatal(err)
	}
	muxB, err := routerB.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	// a client connected to node A waiting for the OAuth2 redirect
	client := subscriptions.NewDefaultClientWithIdPrefix(nodeA.Cluster().NodeId() + "_")
	client.Subscribe("@oauth2")
	nodeA.SubscriptionsBroker().Register(client)

	received := make(chan subscriptions.Message, 1)
	go func() {
		if msg, ok := receiveRealtimeMessage(t, client, 10*time.Second); ok {
			received <- msg
		}
	}()

	// the OAuth2 provider redirect is received by node B
	req := httptest.NewRequest(http.MethodGet, "/api/oauth2-redirect?state="+client.Id()+"&code=123", nil)
	rec := httptest.NewRecorder()

	muxB.ServeHTTP(rec, req)

	if rec.Code >= 400 {
		t.Fatalf("Expected a success response, got %d: %s", rec.Code, rec.Body.String())
	}

	if location := rec.Header().Get("Location"); strings.Contains(location, "failure") {
		t.Fatalf("Expected success redirect, got %q", location)
	}

	select {
	case msg := <-received:
		if msg.Name != "@oauth2" || !strings.Contains(string(msg.Data), `"code":"123"`) {
			t.Fatalf("Unexpected OAuth2 message %s: %s", msg.Name, msg.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Expected the node A client to receive the OAuth2 message")
	}

	if client.HasSubscription("@oauth2") {
		t.Fatal("Expected the @oauth2 subscription to be removed")
	}
}

func TestRealtimeClusterRecreatedCollection(t *testing.T) {
	t.Parallel()

	testDB, err := tests.NewFixturesTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer tests.DropTestDatabase(testDB.Name)

	// a single connection so that the cached prepared statements are reused
	newNode := func() *tests.TestApp {
		app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
			DatabaseURL:      testDB.URL,
			ClusterMode:      true,
			DataMaxOpenConns: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Cleanup)

		if _, err := apis.NewRouter(app); err != nil {
			t.Fatal(err)
		}
		<-app.Cluster().Ready()

		return app
	}

	nodeA := newNode()
	nodeB := newNode()

	client := subscriptions.NewDefaultClient()
	client.Subscribe("cluster_rc/*")
	nodeA.SubscriptionsBroker().Register(client)

	for i := range 2 {
		// (re)create the collection
		if existing, err := nodeB.FindCollectionByNameOrId("cluster_rc"); err == nil {
			if err := nodeB.Delete(existing); err != nil {
				t.Fatal(err)
			}
		}

		collection := core.NewBaseCollection("cluster_rc")
		collection.ListRule = new(string)
		collection.ViewRule = new(string)
		collection.Fields.Add(&core.TextField{Name: "title"})
		if err := nodeB.Save(collection); err != nil {
			t.Fatal(err)
		}

		record := core.NewRecord(collection)
		record.Set("title", "test")
		if err := nodeB.Save(record); err != nil {
			t.Fatal(err)
		}
		expectRealtimeMessage(t, client, "create", record.Id)

		if err := nodeB.Delete(record); err != nil {
			t.Fatal(err)
		}
		expectRealtimeMessage(t, client, "delete", record.Id)

		t.Logf("iteration %d completed", i)
	}
}
