package apis_test

import (
	"encoding/json/v2"
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
