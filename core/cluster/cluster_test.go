package cluster_test

import (
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
)

func newTestCluster(t *testing.T, url string) *cluster.Cluster {
	return newTestClusterWithDB(t, url, nil)
}

const testSecret = "test_cluster_secret"

func newTestClusterWithDB(t *testing.T, url string, db dbx.Builder) *cluster.Cluster {
	return newTestClusterWithConfig(t, cluster.Config{
		DSN:    func() string { return url },
		DB:     func() dbx.Builder { return db },
		Secret: func() (string, error) { return testSecret, nil },
	})
}

func newTestClusterWithConfig(t *testing.T, config cluster.Config) *cluster.Cluster {
	config.Logger = func() *slog.Logger { return slog.Default() }

	c := cluster.New(config)
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
	c.OnResync(func() {
		select {
		case reconnected <- struct{}{}:
		default:
		}
	})

	received := make(chan cluster.Message, 10)
	c.Subscribe("test", func(msg cluster.Message) { received <- msg })

	c.Start()
	waitReady(t, c)

	// the resync after the first LISTEN
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("Expected the resync handler to be called after the first connection")
	}

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

func TestClusterSignedMessages(t *testing.T) {
	t.Parallel()

	testDB, db := openTestDB(t)

	c := newTestCluster(t, testDB.URL)

	// a node with a different secret
	other := newTestClusterWithConfig(t, cluster.Config{
		DSN:    func() string { return testDB.URL },
		DB:     func() dbx.Builder { return db },
		Secret: func() (string, error) { return "other_secret", nil },
	})

	received := make(chan cluster.Message, 10)
	c.Subscribe("test", func(msg cluster.Message) { received <- msg })

	c.Start()
	waitReady(t, c)

	channel, err := c.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(channel, cluster.ChannelPrefix) || strings.Contains(channel, testSecret) {
		t.Fatalf("Expected a channel name derived from the secret, got %q", channel)
	}

	otherChannel, err := other.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if otherChannel == channel {
		t.Fatal("Expected different channels for different secrets")
	}

	// forged messages on the cluster channel (eg. by another role of the database)
	forged := []string{
		"test",
		"n1|test|forged",
		"invalidsignature000000|n1|test|forged",
		strings.Repeat("a", 22) + "|n1|test|forged",
	}
	for _, payload := range forged {
		_, err := db.NewQuery("SELECT pg_notify({:channel}, {:payload})").
			Bind(dbx.Params{"channel": channel, "payload": payload}).
			Execute()
		if err != nil {
			t.Fatal(err)
		}
	}

	// a message of the node with the different secret (sent on the same channel)
	if err := other.Publish(db, "test", "other"); err != nil {
		t.Fatal(err)
	}

	if err := c.Publish(db, "test", "valid"); err != nil {
		t.Fatal(err)
	}

	if msg := receive(t, received); msg.Payload != "valid" || msg.NodeId != c.NodeId() {
		t.Fatalf("Expected only the valid message, got %#v", msg)
	}

	select {
	case msg := <-received:
		t.Fatalf("Expected no other message, got %#v", msg)
	case <-time.After(300 * time.Millisecond):
	}

	if n := c.InvalidMessages(); n != int64(len(forged)) {
		t.Fatalf("Expected %d invalid messages, got %d", len(forged), n)
	}
}

func TestClusterSecretNotReady(t *testing.T) {
	t.Parallel()

	testDB, db := openTestDB(t)

	var ready atomic.Bool

	c := newTestClusterWithConfig(t, cluster.Config{
		DSN: func() string { return testDB.URL },
		DB:  func() dbx.Builder { return db },
		Secret: func() (string, error) {
			if !ready.Load() {
				return "", cluster.ErrNotReady
			}
			return testSecret, nil
		},
	})

	// no node can listen without the secret
	if err := c.Publish(db, "test", "ignored"); err != nil {
		t.Fatalf("Expected no-op publish without the secret, got %v", err)
	}

	received := make(chan cluster.Message, 10)
	c.Subscribe("test", func(msg cluster.Message) { received <- msg })

	c.Start()

	select {
	case <-c.Ready():
		t.Fatal("Expected the cluster to wait for the secret")
	case <-time.After(300 * time.Millisecond):
	}

	ready.Store(true)
	waitReady(t, c)

	if err := c.Publish(db, "test", "ready"); err != nil {
		t.Fatal(err)
	}

	if msg := receive(t, received); msg.Payload != "ready" {
		t.Fatalf("Expected the message after the secret is available, got %#v", msg)
	}
}

func TestClusterDataQueue(t *testing.T) {
	t.Parallel()

	testDB, db := openTestDB(t)

	c := newTestCluster(t, testDB.URL)

	unblock := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	})

	data := make(chan cluster.Message, 10)
	c.SubscribeData("data", func(msg cluster.Message) {
		<-unblock
		data <- msg
	})

	state := make(chan cluster.Message, 10)
	c.Subscribe("state", func(msg cluster.Message) { state <- msg })

	c.Start()
	waitReady(t, c)

	if err := c.Publish(db, "data", "slow"); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(db, "state", "fast"); err != nil {
		t.Fatal(err)
	}

	// the blocked data handler doesn't delay the state messages
	if msg := receive(t, state); msg.Payload != "fast" {
		t.Fatalf("Expected the state message, got %#v", msg)
	}

	close(unblock)

	if msg := receive(t, data); msg.Payload != "slow" {
		t.Fatalf("Expected the data message, got %#v", msg)
	}
}

func TestClusterResync(t *testing.T) {
	t.Parallel()

	t.Run("periodic", func(t *testing.T) {
		t.Parallel()

		testDB, db := openTestDB(t)

		c := newTestClusterWithConfig(t, cluster.Config{
			DSN:            func() string { return testDB.URL },
			DB:             func() dbx.Builder { return db },
			Secret:         func() (string, error) { return testSecret, nil },
			ResyncInterval: 100 * time.Millisecond,
		})

		var calls atomic.Int64
		c.OnResync(func() { calls.Add(1) })

		c.Start()
		waitReady(t, c)

		deadline := time.Now().Add(10 * time.Second)
		for calls.Load() < 4 {
			if time.Now().After(deadline) {
				t.Fatalf("Expected periodic resync calls, got %d", calls.Load())
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("dropped state message", func(t *testing.T) {
		t.Parallel()

		testDB, db := openTestDB(t)

		c := newTestCluster(t, testDB.URL)

		var calls atomic.Int64
		c.OnResync(func() { calls.Add(1) })

		unblock := make(chan struct{})
		var handled atomic.Int64
		c.Subscribe("state", func(msg cluster.Message) {
			<-unblock
			handled.Add(1)
		})

		c.Start()
		waitReady(t, c)

		// the resync after the first LISTEN
		deadline := time.Now().Add(10 * time.Second)
		for calls.Load() < 1 {
			if time.Now().After(deadline) {
				t.Fatal("Expected the initial resync")
			}
			time.Sleep(20 * time.Millisecond)
		}
		initialCalls := calls.Load()

		// fill the queue of the blocked handler (the notifications of a
		// transaction are delivered after its commit and in order)
		const total = 1100
		err := db.Transactional(func(tx *dbx.Tx) error {
			for i := range total {
				if err := c.Publish(tx, "state", fmt.Sprint(i)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		deadline = time.Now().Add(10 * time.Second)
		for c.DroppedMessages() == 0 || calls.Load() == initialCalls {
			if time.Now().After(deadline) {
				t.Fatalf("Expected dropped messages and a resync, got %d dropped and %d resync calls", c.DroppedMessages(), calls.Load())
			}
			time.Sleep(20 * time.Millisecond)
		}

		close(unblock)
	})
}
