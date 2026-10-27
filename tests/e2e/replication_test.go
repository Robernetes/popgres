package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tests"
	"github.com/Robernetes/popgres/tools/list"
	"github.com/Robernetes/popgres/tools/security"
	"github.com/pocketbase/dbx"
)

// newReplicationCluster starts the specified number of app instances (nodes)
// with the replication realtime source that share the same test database,
// and returns them together with a direct database connection (that doesn't use the app API).
func newReplicationCluster(t *testing.T, total int) ([]*clusterNode, *dbx.DB) {
	t.Helper()

	testDB, err := tests.NewFixturesTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tests.DropTestDatabase(testDB.Name) })

	nodes := make([]*clusterNode, total)
	for i := range nodes {
		nodes[i] = startNode(t, core.BaseAppConfig{
			DatabaseURL:    testDB.URL,
			ClusterMode:    true,
			RealtimeSource: cluster.RealtimeSourceReplication,
		})

		if source := nodes[i].app.Cluster().RealtimeSource(); source != cluster.RealtimeSourceReplication {
			t.Fatalf("Expected the replication realtime source, got %q", source)
		}
	}

	db, err := dbx.Open("pgx", testDB.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// wait for the leader to decode a warm-up change
	//
	// note: the logical decoding starts only after a consistent point of the server
	// transactions, which could be delayed by up to 15s (the interval of the running
	// transactions WAL record of an idle server) if there was a running transaction
	// in any database of the server when the slot was created or the decoding started
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := db.NewQuery("UPDATE {{_superusers}} SET [[updated]] = [[updated]] WHERE [[id]] = (SELECT [[id]] FROM {{_superusers}} LIMIT 1)").Execute()
		if err != nil {
			t.Fatal(err)
		}

		var total int
		if err := db.Select("count(*)").From(cluster.RealtimeEventsTable).Row(&total); err != nil {
			t.Fatal(err)
		}
		if total > 0 {
			break
		}

		if time.Now().After(deadline) {
			// the long running transactions of the server delay the logical decoding start
			var activity []string
			_ = db.NewQuery(`
				SELECT [[datname]] || ' ' || COALESCE([[state]], '') || ' ' || (now() - [[xact_start]])::text || ' ' || left(COALESCE([[query]], ''), 80)
				FROM pg_stat_activity
				WHERE [[xact_start]] IS NOT NULL AND [[xact_start]] < now() - interval '5 seconds'
			`).Column(&activity)

			var slots []string
			_ = db.NewQuery(`
				SELECT [[slot_name]] || ' active=' || [[active]]::text || ' restart=' || COALESCE([[restart_lsn]]::text, '') || ' confirmed=' || COALESCE([[confirmed_flush_lsn]]::text, '')
				FROM pg_replication_slots
			`).Column(&slots)

			t.Fatalf("Expected the replication leader to decode the warm-up change\nlong transactions: %v\nslots: %v", activity, slots)
		}
		time.Sleep(200 * time.Millisecond)
	}

	return nodes, db
}

func startNode(t *testing.T, config core.BaseAppConfig) *clusterNode {
	t.Helper()

	app, err := tests.NewTestAppWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	node := newClusterNode(t, app)

	return node
}

func subscribe(t *testing.T, node *clusterNode, client *sseClient, subscriptions ...string) {
	t.Helper()

	raw, _ := json.Marshal(map[string]any{"clientId": client.clientId, "subscriptions": subscriptions})

	status, body := doRequest(t, http.MethodPost, node.srv.URL+"/api/realtime", string(raw), "")
	if status != http.StatusNoContent {
		t.Fatalf("Failed to subscribe: %d %s", status, body)
	}
}

// recordEvents reads the record events of the client until the timeout
// (or until the expected number of events) and returns them as "action:id" items.
func recordEvents(t *testing.T, client *sseClient, expected int, timeout time.Duration) []string {
	t.Helper()

	result := []string{}
	deadline := time.Now().Add(timeout)

	for len(result) < expected {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		e := client.next(t, remaining)
		if e == nil {
			break
		}

		data := struct {
			Action string         `json:"action"`
			Record map[string]any `json:"record"`
		}{}
		if err := json.Unmarshal([]byte(e.data), &data); err != nil {
			t.Fatal(err)
		}

		result = append(result, fmt.Sprintf("%s:%v", data.Action, data.Record["id"]))
	}

	return result
}

func TestReplicationDirectChanges(t *testing.T) {
	t.Parallel()

	nodes, db := newReplicationCluster(t, 2)

	// guest clients (demo2 is public, demo3 requires a non-users auth record)
	allowed := connectRealtime(t, nodes[0].srv.URL)
	subscribe(t, nodes[0], allowed, "demo2/*")

	denied := connectRealtime(t, nodes[1].srv.URL)
	subscribe(t, nodes[1], denied, "demo3/*")

	// direct SQL update (outside of the app API)
	_, err := db.NewQuery("UPDATE {{demo2}} SET [[title]] = 'direct_update' WHERE [[id]] = {:id}").
		Bind(dbx.Params{"id": "achvryl401bhse3"}).
		Execute()
	if err != nil {
		t.Fatal(err)
	}

	if events := recordEvents(t, allowed, 1, 5*time.Second); len(events) != 1 || events[0] != "update:achvryl401bhse3" {
		t.Fatalf("Expected the direct update event, got %v", events)
	}

	_, err = db.NewQuery("UPDATE {{demo3}} SET [[title]] = 'direct_update' WHERE [[id]] = {:id}").
		Bind(dbx.Params{"id": "mk5fmymtx4wsprk"}).
		Execute()
	if err != nil {
		t.Fatal(err)
	}

	if events := recordEvents(t, denied, 1, time.Second); len(events) != 0 {
		t.Fatalf("Expected no events for the client without access, got %v", events)
	}

	// direct SQL delete
	_, err = db.NewQuery("DELETE FROM {{demo2}} WHERE [[id]] = {:id}").
		Bind(dbx.Params{"id": "achvryl401bhse3"}).
		Execute()
	if err != nil {
		t.Fatal(err)
	}

	if events := recordEvents(t, allowed, 1, 5*time.Second); len(events) != 1 || events[0] != "delete:achvryl401bhse3" {
		t.Fatalf("Expected the direct delete event, got %v", events)
	}

	// a change through the API is broadcasted only once (from the replication)
	status, body := doRequest(t, http.MethodPost, nodes[1].srv.URL+"/api/collections/demo2/records", `{"title":"api"}`, "")
	if status != http.StatusOK {
		t.Fatalf("Failed to create record: %d %s", status, body)
	}
	record := map[string]any{}
	_ = json.Unmarshal([]byte(body), &record)

	events := recordEvents(t, allowed, 2, 2*time.Second)
	if len(events) != 1 || events[0] != fmt.Sprintf("create:%v", record["id"]) {
		t.Fatalf("Expected a single create event, got %v", events)
	}

	// a single transaction with more changes than a batch of the leader
	const total = 1200

	var ids []string
	err = db.NewQuery("INSERT INTO {{demo2}} ([[title]]) SELECT 'bulk_' || i FROM generate_series(1, {:total}) i RETURNING [[id]]").
		Bind(dbx.Params{"total": total}).
		Column(&ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != total {
		t.Fatalf("Expected %d inserted records, got %d", total, len(ids))
	}

	events = recordEvents(t, allowed, total, 60*time.Second)
	events = append(events, recordEvents(t, allowed, 1, time.Second)...) // eventual duplicates

	counts := map[string]int{}
	for _, e := range events {
		counts[e]++
	}
	for _, id := range ids {
		if counts["create:"+id] != 1 {
			t.Fatalf("Expected each change exactly once, got %d events (record %s: %d)", len(events), id, counts["create:"+id])
		}
	}

	// the slot can be synchronized to the standbys and its creation is recorded
	var failover bool
	err = db.NewQuery("SELECT [[failover]] FROM pg_replication_slots WHERE [[database]] = current_database()").Row(&failover)
	if err != nil {
		t.Fatal(err)
	}
	if !failover {
		t.Fatal("Expected the failover option of the realtime slot")
	}

	var recorded int
	if err := db.Select("count(*)").From("_params").Where(dbx.HashExp{"id": "popgresRealtimeSlot"}).Row(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Fatal("Expected the realtime slot creation to be recorded")
	}
}

func TestReplicationLeaderFailover(t *testing.T) {
	t.Parallel()

	nodes, db := newReplicationCluster(t, 2)

	var leader, follower *clusterNode
	for _, node := range nodes {
		if node.app.Cluster().IsReplicationLeader() {
			leader = node
		} else {
			follower = node
		}
	}
	if leader == nil || follower == nil {
		t.Fatal("Expected a leader and a follower")
	}

	client := connectRealtime(t, follower.srv.URL)
	subscribe(t, follower, client, "demo2/*")

	const total = 500

	inserted := make(chan []string, 1)
	go func() {
		ids := make([]string, 0, total)
		for i := range total {
			var id string
			err := db.NewQuery("INSERT INTO {{demo2}} ([[title]]) VALUES ({:title}) RETURNING [[id]]").
				Bind(dbx.Params{"title": fmt.Sprintf("burst_%d", i)}).
				Row(&id)
			if err != nil {
				t.Error(err)
				break
			}
			ids = append(ids, id)
		}
		inserted <- ids
	}()

	// stop the leader during the burst
	time.Sleep(300 * time.Millisecond)
	leader.app.Cluster().StopReplication()

	ids := <-inserted
	if len(ids) != total {
		t.Fatalf("Expected %d inserted records, got %d", total, len(ids))
	}

	// note: under load the new leader could need up to ~15s to restart the
	// decoding (the running transactions WAL record of an idle server), so
	// the deadline is generous and the late events are not reported as lost
	events := recordEvents(t, client, total, 90*time.Second)
	events = append(events, recordEvents(t, client, 1, time.Second)...) // eventual duplicates

	counts := map[string]int{}
	for _, e := range events {
		counts[e]++
	}

	var missing, duplicated []string
	for _, id := range ids {
		switch counts["create:"+id] {
		case 0:
			missing = append(missing, id)
		case 1:
		default:
			duplicated = append(duplicated, id)
		}
	}

	if len(missing) > 0 || len(duplicated) > 0 {
		// diagnostics: whether the missing changes were decoded and stored by a leader
		var stored int
		if len(missing) > 0 {
			_ = db.Select("count(*)").From("_realtime_events").Where(dbx.In("record_id", list.ToInterfaceSlice(missing)...)).Row(&stored)
		}
		t.Fatalf(
			"Expected each change exactly once, got %d missing (%d of them stored in _realtime_events: %v) and %d duplicated (%d events); follower dropped messages %d, failed broadcasts %d",
			len(missing), stored, missing, len(duplicated), len(events),
			follower.app.Cluster().DroppedMessages(), follower.app.Cluster().RealtimeFailures(),
		)
	}

	if !follower.app.Cluster().IsReplicationLeader() {
		t.Fatal("Expected the follower to become the replication leader")
	}
}

func TestReplicationLatency(t *testing.T) {
	t.Parallel()

	nodes, db := newReplicationCluster(t, 1)

	client := connectRealtime(t, nodes[0].srv.URL)
	subscribe(t, nodes[0], client, "demo2/achvryl401bhse3")

	const total = 200

	latencies := make([]time.Duration, 0, total)

	for i := range total {
		start := time.Now()

		_, err := db.NewQuery("UPDATE {{demo2}} SET [[title]] = {:title} WHERE [[id]] = {:id}").
			Bind(dbx.Params{"id": "achvryl401bhse3", "title": fmt.Sprintf("latency_%d", i)}).
			Execute()
		if err != nil {
			t.Fatal(err)
		}

		if events := recordEvents(t, client, 1, 5*time.Second); len(events) != 1 {
			t.Fatalf("[%d] Expected the update event", i)
		}

		latencies = append(latencies, time.Since(start))
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[total*95/100-1]

	t.Logf("realtime replication latency p50 %v, p95 %v", latencies[total/2], p95)

	if p95 >= 300*time.Millisecond {
		t.Fatalf("Expected p95 latency < 300ms, got %v", p95)
	}
}

func TestReplicationFallbackWithoutReplicationRole(t *testing.T) {
	t.Parallel()

	testDB, err := tests.NewFixturesTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer tests.DropTestDatabase(testDB.Name)

	db, err := dbx.Open("pgx", testDB.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// a role without the REPLICATION attribute and with access to the test database
	role := "popgres_t_norepl_" + security.RandomStringWithAlphabet(8, "abcdefghijklmnopqrstuvwxyz0123456789")
	password := security.RandomString(20)

	setup := []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOREPLICATION PASSWORD '%s'", role, password),
		fmt.Sprintf("GRANT ALL ON SCHEMA public, _aux TO %s", role),
		fmt.Sprintf("GRANT ALL ON ALL TABLES IN SCHEMA public, _aux TO %s", role),
		fmt.Sprintf("GRANT ALL ON ALL SEQUENCES IN SCHEMA public, _aux TO %s", role),
	}
	for _, q := range setup {
		if _, err := db.NewQuery(q).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		cleanup := []string{
			fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA public, _aux FROM %s", role),
			fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA public, _aux FROM %s", role),
			fmt.Sprintf("REVOKE ALL ON SCHEMA public, _aux FROM %s", role),
			fmt.Sprintf("DROP ROLE IF EXISTS %s", role),
		}
		for _, q := range cleanup {
			if _, err := db.NewQuery(q).Execute(); err != nil {
				t.Errorf("Failed to cleanup the test role: %v", err)
			}
		}
	}()

	u, err := url.Parse(testDB.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)

	// note: a regular app (the test apps don't store their logs)
	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:        t.TempDir(),
		DatabaseURL:    u.String(),
		RealtimeSource: cluster.RealtimeSourceReplication,
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}

	source := app.Cluster().RealtimeSource()

	// flushes the logs
	if err := app.ClearBootstrap(); err != nil {
		t.Fatal(err)
	}

	if source != cluster.RealtimeSourceHooks {
		t.Fatalf("Expected the hooks realtime source, got %q", source)
	}

	var total int
	err = db.Select("count(*)").
		From("_aux." + core.LogsTableName).
		Where(dbx.NewExp("strpos([[message]], 'replication realtime source is not available') > 0")).
		Row(&total)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("Expected the replication fallback warning to be logged")
	}
}
