package cluster_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
)

func newTestCluster(t *testing.T, url string) *cluster.Cluster {
	c := cluster.New(
		func() string { return url },
		func() *slog.Logger { return slog.Default() },
	)
	t.Cleanup(c.Stop)

	return c
}

func waitReady(t *testing.T, c *cluster.Cluster) {
	select {
	case <-c.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("cluster LISTEN connection timeout")
	}
}

func receive(t *testing.T, ch chan cluster.Message) cluster.Message {
	select {
	case msg := <-ch:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("cluster message timeout")
	}
	return cluster.Message{}
}

func openTestDB(t *testing.T) (*tests.TestDatabase, *dbx.DB) {
	testDB, err := tests.NewTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tests.DropTestDatabase(testDB.Name) })

	db, err := dbx.Open("pgx", testDB.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	return testDB, db
}

func TestClusterPublishSubscribe(t *testing.T) {
	t.Parallel()

	testDB, db := openTestDB(t)

	a := newTestCluster(t, testDB.URL)
	b := newTestCluster(t, testDB.URL)

	if a.NodeId() == "" || a.NodeId() == b.NodeId() {
		t.Fatalf("Expected unique non-empty node ids, got %q and %q", a.NodeId(), b.NodeId())
	}

	received := make(chan cluster.Message, 10)
	b.Subscribe("test", func(msg cluster.Message) { received <- msg })

	other := make(chan cluster.Message, 10)
	unsubscribe := b.Subscribe("other", func(msg cluster.Message) { other <- msg })
	unsubscribe()

	a.Start()
	b.Start()
	waitReady(t, a)
	waitReady(t, b)

	if err := a.Publish(db, "other", "ignored"); err != nil {
		t.Fatal(err)
	}
	if err := a.Publish(db, "test", "id1|id2"); err != nil {
		t.Fatal(err)
	}

	msg := receive(t, received)
	if msg.NodeId != a.NodeId() || msg.Topic != "test" || msg.Payload != "id1|id2" {
		t.Fatalf("Unexpected message %#v", msg)
	}

	select {
	case msg := <-other:
		t.Fatalf("Expected no message for the unsubscribed handler, got %#v", msg)
	default:
	}

	// a message published inside a transaction is delivered after the commit
	err := db.Transactional(func(tx *dbx.Tx) error {
		if err := a.Publish(tx, "test", "tx"); err != nil {
			return err
		}

		select {
		case msg := <-received:
			t.Fatalf("Expected the message to be delivered after the commit, got %#v", msg)
		case <-time.After(300 * time.Millisecond):
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if msg := receive(t, received); msg.Payload != "tx" {
		t.Fatalf("Expected the tx message, got %#v", msg)
	}
}

func TestClusterPublishValidation(t *testing.T) {
	t.Parallel()

	testDB, db := openTestDB(t)

	c := newTestCluster(t, testDB.URL)

	if err := c.Publish(db, "", "test"); err == nil {
		t.Fatal("Expected empty topic error")
	}

	if err := c.Publish(db, "a|b", "test"); err == nil {
		t.Fatal("Expected invalid topic error")
	}

	if err := c.Publish(db, "test", strings.Repeat("a", cluster.MaxPayloadSize+1)); err == nil {
		t.Fatal("Expected payload size error")
	}

	if err := c.Publish(db, "test", strings.Repeat("a", cluster.MaxPayloadSize)); err != nil {
		t.Fatalf("Expected max size payload to be published, got %v", err)
	}
}

func TestClusterReconnect(t *testing.T) {
	t.Parallel()

	testDB, db := openTestDB(t)

	c := newTestCluster(t, testDB.URL)

	reconnected := make(chan struct{}, 1)
	c.OnReconnect(func() {
		select {
		case reconnected <- struct{}{}:
		default:
		}
	})

	received := make(chan cluster.Message, 10)
	c.Subscribe("test", func(msg cluster.Message) { received <- msg })

	c.Start()
	waitReady(t, c)

	// kill the LISTEN connection
	_, err := db.NewQuery(`
		SELECT pg_terminate_backend([[pid]]) FROM pg_stat_activity
		WHERE [[datname]] = current_database() AND [[pid]] <> pg_backend_pid() AND [[query]] ILIKE 'LISTEN%'
	`).Execute()
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("Expected the reconnect handler to be called")
	}

	if err := c.Publish(db, "test", "after"); err != nil {
		t.Fatal(err)
	}

	if msg := receive(t, received); msg.Payload != "after" {
		t.Fatalf("Expected the message after the reconnect, got %#v", msg)
	}
}

func TestClusterStopStart(t *testing.T) {
	t.Parallel()

	testDB, db := openTestDB(t)

	c := newTestCluster(t, testDB.URL)

	received := make(chan cluster.Message, 10)
	c.Subscribe("test", func(msg cluster.Message) { received <- msg })

	// no-op
	c.Stop()

	c.Start()
	c.Start() // no-op
	waitReady(t, c)
	c.Stop()

	c.Start()
	waitReady(t, c)

	if err := c.Publish(db, "test", "restarted"); err != nil {
		t.Fatal(err)
	}

	if msg := receive(t, received); msg.Payload != "restarted" {
		t.Fatalf("Expected the message after the restart, got %#v", msg)
	}
}
