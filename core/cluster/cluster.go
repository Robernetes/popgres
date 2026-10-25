// Package cluster implements the coordination of the popgres app instances
// (nodes) that share the same Postgres database.
//
// The nodes exchange short messages with LISTEN/NOTIFY. All messages are sent
// on a single Postgres channel ([Channel]) and are routed to the subscribers
// by their topic, so that new topics don't require a new LISTEN.
//
// The NOTIFY payloads are limited to 8000 bytes by Postgres, so the messages
// are expected to carry only ids (the larger data should be stored in a table).
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Robernetes/popgres/tools/security"
	"github.com/jackc/pgx/v5"
	"github.com/pocketbase/dbx"
)

// Channel is the Postgres LISTEN/NOTIFY channel used by the cluster messages.
const Channel = "popgres_cluster"

// MaxPayloadSize is the max size in bytes of a message payload
// (the Postgres NOTIFY limit minus the message header).
const MaxPayloadSize = 7000

const (
	minReconnectDelay = 100 * time.Millisecond
	maxReconnectDelay = 10 * time.Second

	dispatchQueueSize = 1024
)

// Message is a cluster message received from a node (including the current one).
type Message struct {
	// NodeId is the id of the node that published the message.
	NodeId string

	// Topic is the message topic (eg. "collections").
	Topic string

	// Payload is the optional message data.
	Payload string
}

// Handler handles the messages of a subscribed topic.
type Handler func(msg Message)

// Cluster manages the LISTEN connection of a node and the dispatch
// of the received messages to the topic subscribers.
//
// The LISTEN connection is dedicated (it is not part of the app pool) and it
// is automatically reconnected with a backoff in case of an error.
type Cluster struct {
	nodeId string
	dsn    func() string
	logger func() *slog.Logger
	kv     *KV

	mu                sync.RWMutex
	handlers          map[string]map[string]Handler
	reconnectHandlers map[string]func()

	runMu   sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	ready   chan struct{}
	queue   chan func()
	running bool
}

// Config defines the Cluster dependencies.
//
// They are functions because they could change on app bootstrap.
type Config struct {
	// DSN returns the connection string of the shared database
	// (used for the dedicated LISTEN connection).
	DSN func() string

	// DB returns the main database pool (not a transaction).
	DB func() dbx.Builder

	// Logger returns the logger to use.
	Logger func() *slog.Logger
}

// New creates a new Cluster instance.
func New(config Config) *Cluster {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default
	}

	return &Cluster{
		nodeId:            "n" + security.RandomStringWithAlphabet(11, "abcdefghijklmnopqrstuvwxyz0123456789"),
		dsn:               config.DSN,
		logger:            logger,
		kv:                newKV(config.DB),
		handlers:          map[string]map[string]Handler{},
		reconnectHandlers: map[string]func(){},
		ready:             make(chan struct{}),
	}
}

// KV returns the key-value store with expiration shared by all nodes.
func (c *Cluster) KV() *KV {
	return c.kv
}

// NodeId returns the unique id of the current node (app instance).
func (c *Cluster) NodeId() string {
	return c.nodeId
}

// Subscribe registers a handler for the messages of the specified topic
// and returns a function that removes it.
//
// The handlers are called sequentially from a single dispatch goroutine,
// so they should not block for long. The messages published by the
// current node are delivered too (check [Message.NodeId] to skip them).
func (c *Cluster) Subscribe(topic string, handler Handler) (unsubscribe func()) {
	id := security.PseudorandomString(10)

	c.mu.Lock()
	if c.handlers[topic] == nil {
		c.handlers[topic] = map[string]Handler{}
	}
	c.handlers[topic][id] = handler
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		delete(c.handlers[topic], id)
		c.mu.Unlock()
	}
}

// OnReconnect registers a function that is called after the LISTEN connection
// was reestablished (the messages published in the meantime are lost, so
// the subscribers should resync their state) and returns a function that removes it.
func (c *Cluster) OnReconnect(fn func()) (unsubscribe func()) {
	id := security.PseudorandomString(10)

	c.mu.Lock()
	c.reconnectHandlers[id] = fn
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		delete(c.reconnectHandlers, id)
		c.mu.Unlock()
	}
}

// Publish sends a message to all nodes (including the current one).
//
// The message is sent with pg_notify using the provided db builder. If the
// builder is a transaction, the message is delivered only after its commit.
func (c *Cluster) Publish(db dbx.Builder, topic string, payload string) error {
	if topic == "" || strings.Contains(topic, "|") {
		return fmt.Errorf("invalid cluster message topic %q", topic)
	}

	if len(payload) > MaxPayloadSize {
		return fmt.Errorf("the cluster message payload exceeds %d bytes", MaxPayloadSize)
	}

	_, err := db.NewQuery("SELECT pg_notify({:channel}, {:payload})").Bind(dbx.Params{
		"channel": Channel,
		"payload": c.nodeId + "|" + topic + "|" + payload,
	}).Execute()

	return err
}

// Ready returns a channel that is closed after the first successful LISTEN.
func (c *Cluster) Ready() <-chan struct{} {
	c.runMu.Lock()
	defer c.runMu.Unlock()

	return c.ready
}

// Start starts listening for the cluster messages in the background.
//
// It is no-op if the cluster is already started.
func (c *Cluster) Start() {
	c.runMu.Lock()
	defer c.runMu.Unlock()

	if c.running {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	c.running = true
	c.cancel = cancel
	c.done = make(chan struct{})
	c.queue = make(chan func(), dispatchQueueSize)
	select {
	case <-c.ready:
		// closed by a previous run
		c.ready = make(chan struct{})
	default:
	}

	go c.dispatch(c.queue)
	go c.run(ctx, c.done, c.ready, c.queue)
}

// Stop closes the LISTEN connection and waits for the background goroutines to finish.
//
// It is no-op if the cluster is not started.
func (c *Cluster) Stop() {
	c.runMu.Lock()
	defer c.runMu.Unlock()

	if !c.running {
		return
	}

	c.cancel()
	<-c.done
	close(c.queue)

	c.running = false
}

func (c *Cluster) run(ctx context.Context, done chan struct{}, ready chan struct{}, queue chan func()) {
	defer close(done)

	delay := minReconnectDelay
	connectedBefore := false

	for {
		err := c.listen(ctx, ready, queue, connectedBefore, func() {
			connectedBefore = true
			delay = minReconnectDelay
		})
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			c.logger().Warn(
				"Cluster LISTEN connection failure, reconnecting...",
				slog.String("error", err.Error()),
				slog.String("retryIn", delay.String()),
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		delay = min(delay*2, maxReconnectDelay)
	}
}

func (c *Cluster) listen(
	ctx context.Context,
	ready chan struct{},
	queue chan func(),
	reconnect bool,
	onConnected func(),
) error {
	var dsn string
	if c.dsn != nil {
		dsn = c.dsn()
	}
	if dsn == "" {
		return errors.New("missing database connection string")
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{Channel}.Sanitize()); err != nil {
		return err
	}

	onConnected()

	select {
	case <-ready:
	default:
		close(ready)
	}

	if reconnect {
		c.mu.RLock()
		for _, fn := range c.reconnectHandlers {
			c.enqueue(queue, fn)
		}
		c.mu.RUnlock()
	}

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}

		msg, ok := parseMessage(n.Payload)
		if !ok {
			c.logger().Warn("Invalid cluster message", slog.String("payload", n.Payload))
			continue
		}

		c.mu.RLock()
		for _, h := range c.handlers[msg.Topic] {
			c.enqueue(queue, func() { h(msg) })
		}
		c.mu.RUnlock()
	}
}

// enqueue adds fn to the dispatch queue (dropping it with a warning if the queue is full
// so that a slow handler doesn't block the LISTEN connection).
func (c *Cluster) enqueue(queue chan func(), fn func()) {
	select {
	case queue <- fn:
	default:
		c.logger().Warn("Cluster dispatch queue is full, dropping a message")
	}
}

func (c *Cluster) dispatch(queue chan func()) {
	for fn := range queue {
		c.safeCall(fn)
	}
}

func (c *Cluster) safeCall(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			c.logger().Error("Cluster message handler panic", slog.Any("error", r))
		}
	}()

	fn()
}

func parseMessage(raw string) (Message, bool) {
	parts := strings.SplitN(raw, "|", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return Message{}, false
	}

	return Message{NodeId: parts[0], Topic: parts[1], Payload: parts[2]}, true
}
