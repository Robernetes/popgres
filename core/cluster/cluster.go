// Package cluster implements the coordination of the popgres app instances
// (nodes) that share the same Postgres database.
//
// The nodes exchange short messages with LISTEN/NOTIFY. All messages are sent
// on a single Postgres channel and are routed to the subscribers by their
// topic, so that new topics don't require a new LISTEN.
//
// Postgres doesn't restrict LISTEN and NOTIFY with privileges (any role that can
// connect to the database can use them), so the channel name is derived from a
// secret stored in the database (see [Config.Secret]) and each message is signed
// with it. The messages with an invalid signature are ignored.
//
// The NOTIFY payloads are limited to 8000 bytes by Postgres, so the messages
// are expected to carry only ids (the larger data should be stored in a table).
package cluster

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Robernetes/popgres/tools/security"
	"github.com/jackc/pgx/v5"
	"github.com/pocketbase/dbx"
)

// ChannelPrefix is the prefix of the Postgres LISTEN/NOTIFY channel used by the
// cluster messages (the rest of the name is derived from the cluster secret).
const ChannelPrefix = "popgres_cluster_"

// MaxPayloadSize is the max size in bytes of a message payload
// (the Postgres NOTIFY limit minus the message header).
const MaxPayloadSize = 7000

// DefaultResyncInterval is the default interval of the periodic resync (see [Cluster.OnResync]).
const DefaultResyncInterval = time.Minute

// ErrNotReady is returned by [Config.Secret] when the secret can't be loaded
// yet (eg. before the system migrations of a new database).
var ErrNotReady = errors.New("the cluster secret is not available yet")

const (
	minReconnectDelay = 100 * time.Millisecond
	maxReconnectDelay = 10 * time.Second

	dispatchQueueSize = 1024

	// the base64url length of the truncated HMAC-SHA256 message signature (128 bits)
	signatureLength = 22

	// the min interval between the repeated warnings of the same kind (eg. a full queue)
	warnInterval = 10 * time.Second
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

type subscription struct {
	handler Handler
	data    bool
}

// Cluster manages the LISTEN connection of a node and the dispatch
// of the received messages to the topic subscribers.
//
// The LISTEN connection is dedicated (it is not part of the app pool) and it
// is automatically reconnected with a backoff in case of an error.
//
// The received messages are dispatched by two goroutines with separate queues:
// one for the state changes (eg. the collections and settings, see [Cluster.Subscribe])
// and one for the data events that could take longer to handle
// (eg. the realtime events, see [Cluster.SubscribeData]).
type Cluster struct {
	droppedMessages  atomic.Int64
	invalidMessages  atomic.Int64
	realtimeFailures atomic.Int64
	connected        atomic.Bool

	nodeId         string
	dsn            func() string
	secret         func() (string, error)
	logger         func() *slog.Logger
	enabled        func() bool
	resyncInterval time.Duration
	kv             *KV

	keyMu sync.Mutex
	key   []byte

	mu             sync.RWMutex
	handlers       map[string]map[string]subscription
	resyncHandlers map[string]func()
	resyncPending  atomic.Bool

	warnMu    sync.Mutex
	lastWarns map[string]time.Time

	realtimeSource    atomic.Value
	replicationLeader atomic.Bool
	replicationMu     sync.Mutex
	replication       *replicationState

	runMu     sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	ready     chan struct{}
	queue     chan func()
	dataQueue chan func()
	running   bool
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

	// Secret returns the secret shared by all nodes that authenticates the
	// cluster messages. It must be stored in the database (so that the
	// nodes share it) and it must not be readable by the untrusted roles.
	//
	// It should return [ErrNotReady] if it can't be loaded yet.
	Secret func() (string, error)

	// Logger returns the logger to use.
	Logger func() *slog.Logger

	// Enabled reports whether the cluster mode is enabled
	// (see [Cluster.Enabled]).
	Enabled func() bool

	// ResyncInterval is the interval of the periodic resync
	// (default to [DefaultResyncInterval]).
	ResyncInterval time.Duration
}

// New creates a new Cluster instance.
func New(config Config) *Cluster {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default
	}

	resyncInterval := config.ResyncInterval
	if resyncInterval <= 0 {
		resyncInterval = DefaultResyncInterval
	}

	return &Cluster{
		nodeId:         "n" + security.RandomStringWithAlphabet(11, "abcdefghijklmnopqrstuvwxyz0123456789"),
		dsn:            config.DSN,
		secret:         config.Secret,
		logger:         logger,
		enabled:        config.Enabled,
		resyncInterval: resyncInterval,
		kv:             newKV(config.DB),
		handlers:       map[string]map[string]subscription{},
		resyncHandlers: map[string]func(){},
		lastWarns:      map[string]time.Time{},
		ready:          make(chan struct{}),
	}
}

// Enabled reports whether the cluster mode is enabled.
//
// The state synchronization of the nodes (eg. the cached collections and
// the realtime events) works always, while the features with an extra
// cost per request (eg. the shared rate limits) are used only in cluster mode.
func (c *Cluster) Enabled() bool {
	return c.enabled != nil && c.enabled()
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
// The handlers are called sequentially from the state changes dispatch
// goroutine, so they must not block (see also [Cluster.SubscribeData]).
// The messages published by the current node are delivered too
// (check [Message.NodeId] to skip them).
//
// If a message can't be dispatched (the queue is full), the resync
// handlers are called (see [Cluster.OnResync]).
func (c *Cluster) Subscribe(topic string, handler Handler) (unsubscribe func()) {
	return c.subscribe(topic, subscription{handler: handler})
}

// SubscribeData is similar to [Cluster.Subscribe] but the handlers are called
// from a separate dispatch goroutine (and queue) for the data events that could
// take longer to handle (eg. the realtime events), so that they don't delay the
// state changes messages.
//
// The data messages that can't be dispatched (the queue is full) are dropped.
func (c *Cluster) SubscribeData(topic string, handler Handler) (unsubscribe func()) {
	return c.subscribe(topic, subscription{handler: handler, data: true})
}

func (c *Cluster) subscribe(topic string, sub subscription) (unsubscribe func()) {
	id := security.PseudorandomString(10)

	c.mu.Lock()
	if c.handlers[topic] == nil {
		c.handlers[topic] = map[string]subscription{}
	}
	c.handlers[topic][id] = sub
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		delete(c.handlers[topic], id)
		c.mu.Unlock()
	}
}

// OnResync registers a function that resyncs the state of a subscriber
// and returns a function that removes it.
//
// The messages could be lost, so the resync functions are called:
//   - after each (re)connection of the LISTEN connection (the messages
//     published while it was not connected are not received);
//   - after a state changes message is dropped (see [Cluster.Subscribe]);
//   - periodically (see [Config.ResyncInterval]).
//
// They are called in a background goroutine, so they could run concurrently with
// the message handlers (and with themselves if a resync is requested while running).
func (c *Cluster) OnResync(fn func()) (unsubscribe func()) {
	id := security.PseudorandomString(10)

	c.mu.Lock()
	c.resyncHandlers[id] = fn
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		delete(c.resyncHandlers, id)
		c.mu.Unlock()
	}
}

// Channel returns the name of the Postgres LISTEN/NOTIFY channel of the cluster messages.
func (c *Cluster) Channel() (string, error) {
	key, err := c.loadKey()
	if err != nil {
		return "", err
	}

	return channelName(key), nil
}

// Publish sends a message to all nodes (including the current one).
//
// The message is sent with pg_notify using the provided db builder. If the
// builder is a transaction, the message is delivered only after its commit.
//
// It is no-op if the cluster secret is not available yet (no node can listen without it).
func (c *Cluster) Publish(db dbx.Builder, topic string, payload string) error {
	if topic == "" || strings.Contains(topic, "|") {
		return fmt.Errorf("invalid cluster message topic %q", topic)
	}

	if len(payload) > MaxPayloadSize {
		return fmt.Errorf("the cluster message payload exceeds %d bytes", MaxPayloadSize)
	}

	key, err := c.loadKey()
	if err != nil {
		if errors.Is(err, ErrNotReady) {
			return nil
		}
		return err
	}

	data := c.nodeId + "|" + topic + "|" + payload

	_, err = db.NewQuery("SELECT pg_notify({:channel}, {:payload})").Bind(dbx.Params{
		"channel": channelName(key),
		"payload": sign(key, data) + "|" + data,
	}).Execute()

	return err
}

// Notify is similar to [Cluster.Publish] but the message is sent with the
// main database pool (it is not part of the current transaction, if any).
func (c *Cluster) Notify(topic string, payload string) error {
	db, err := c.kv.builder()
	if err != nil {
		return err
	}

	return c.Publish(db, topic, payload)
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
	c.dataQueue = make(chan func(), dispatchQueueSize)
	select {
	case <-c.ready:
		// closed by a previous run
		c.ready = make(chan struct{})
	default:
	}

	go c.dispatch(c.queue)
	go c.dispatch(c.dataQueue)
	go c.run(ctx, c.done, c.ready, c.queue, c.dataQueue)
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
	close(c.dataQueue)

	// the secret is loaded again on the next start (eg. the database could be restored)
	c.keyMu.Lock()
	c.key = nil
	c.keyMu.Unlock()

	c.running = false
}

func (c *Cluster) run(ctx context.Context, done chan struct{}, ready chan struct{}, queue chan func(), dataQueue chan func()) {
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		close(done)
	}()

	// the periodic resync (independent of the LISTEN connection state)
	wg.Add(1)
	go func() {
		defer wg.Done()

		ticker := time.NewTicker(c.resyncInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.requestResync()
			}
		}
	}()

	delay := minReconnectDelay

	for {
		err := c.safeListen(ctx, ready, queue, dataQueue, func() {
			delay = minReconnectDelay
		})
		if ctx.Err() != nil {
			return
		}

		if errors.Is(err, ErrNotReady) {
			// eg. a new database before the system migrations (retried without logging)
			delay = minReconnectDelay
		} else if err != nil {
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

// safeListen calls listen and returns a panic (if any) as an error.
func (c *Cluster) safeListen(
	ctx context.Context,
	ready chan struct{},
	queue chan func(),
	dataQueue chan func(),
	onConnected func(),
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("cluster LISTEN panic: %v", r)
		}
	}()

	return c.listen(ctx, ready, queue, dataQueue, onConnected)
}

func (c *Cluster) listen(
	ctx context.Context,
	ready chan struct{},
	queue chan func(),
	dataQueue chan func(),
	onConnected func(),
) error {
	var dsn string
	if c.dsn != nil {
		dsn = c.dsn()
	}
	if dsn == "" {
		return errors.New("missing database connection string")
	}

	key, err := c.loadKey()
	if err != nil {
		return err
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

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channelName(key)}.Sanitize()); err != nil {
		return err
	}

	onConnected()

	c.connected.Store(true)
	defer c.connected.Store(false)

	select {
	case <-ready:
	default:
		close(ready)
	}

	// the messages published before the LISTEN are not received
	// (eg. while the app was loading its state or while reconnecting)
	c.requestResync()

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}

		msg, ok := parseMessage(key, n.Payload)
		if !ok {
			c.invalidMessages.Add(1)
			c.warnLimited("invalid", "Invalid cluster message (wrong format or signature)")
			continue
		}

		c.mu.RLock()
		for _, sub := range c.handlers[msg.Topic] {
			h := sub.handler
			if sub.data {
				if !c.enqueue(dataQueue, func() { h(msg) }) {
					c.warnLimited("dataQueue", "Cluster data dispatch queue is full, dropping a message")
				}
			} else if !c.enqueue(queue, func() { h(msg) }) {
				c.warnLimited("queue", "Cluster dispatch queue is full, dropping a message and resyncing")
				c.requestResync()
			}
		}
		c.mu.RUnlock()
	}
}

// enqueue adds fn to the dispatch queue (dropping it if the queue is full
// so that a slow handler doesn't block the LISTEN connection).
//
// It reports whether fn was added.
func (c *Cluster) enqueue(queue chan func(), fn func()) bool {
	select {
	case queue <- fn:
		return true
	default:
		c.droppedMessages.Add(1)
		return false
	}
}

// Resync calls the resync handlers in the background (see [Cluster.OnResync]).
func (c *Cluster) Resync() {
	c.requestResync()
}

// requestResync calls the resync handlers in the background (if they are not already
// scheduled to run, eg. after multiple dropped messages).
func (c *Cluster) requestResync() {
	if !c.resyncPending.CompareAndSwap(false, true) {
		return
	}

	go func() {
		// reset before the handlers are called so that a resync
		// requested while they are running is not lost
		c.resyncPending.Store(false)

		c.mu.RLock()
		handlers := make([]func(), 0, len(c.resyncHandlers))
		for _, fn := range c.resyncHandlers {
			handlers = append(handlers, fn)
		}
		c.mu.RUnlock()

		for _, fn := range handlers {
			c.safeCall(fn)
		}
	}()
}

// warnLimited logs a warning with the messages counters (see [Cluster.WarnLimited]).
func (c *Cluster) warnLimited(kind string, message string) {
	c.WarnLimited(
		kind,
		message,
		slog.Int64("droppedMessages", c.DroppedMessages()),
		slog.Int64("invalidMessages", c.InvalidMessages()),
	)
}

// WarnLimited logs a warning at most once per 10 seconds for the same kind
// (eg. so that a flood of invalid messages doesn't flood the logs).
func (c *Cluster) WarnLimited(kind string, message string, args ...any) {
	c.warnMu.Lock()
	last := c.lastWarns[kind]
	now := time.Now()
	if now.Sub(last) < warnInterval {
		c.warnMu.Unlock()
		return
	}
	c.lastWarns[kind] = now
	c.warnMu.Unlock()

	c.logger().Warn(message, args...)
}

// Connected reports whether the LISTEN connection of the node is connected
// (the node doesn't receive the cluster messages while it is disconnected).
func (c *Cluster) Connected() bool {
	return c.connected.Load()
}

// DroppedMessages returns the number of the received messages that were
// dropped because their dispatch queue was full.
func (c *Cluster) DroppedMessages() int64 {
	return c.droppedMessages.Load()
}

// InvalidMessages returns the number of the received messages that were
// ignored because of an invalid format or signature.
func (c *Cluster) InvalidMessages() int64 {
	return c.invalidMessages.Load()
}

// AddRealtimeFailure increments the counter of the realtime replication
// events that couldn't be broadcasted (see [Cluster.RealtimeFailures]).
func (c *Cluster) AddRealtimeFailure() {
	c.realtimeFailures.Add(1)
}

// RealtimeFailures returns the number of the realtime replication events
// that couldn't be broadcasted to the local clients.
func (c *Cluster) RealtimeFailures() int64 {
	return c.realtimeFailures.Load()
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

// loadKey returns the cluster secret key (loading it on the first call).
func (c *Cluster) loadKey() ([]byte, error) {
	c.keyMu.Lock()
	defer c.keyMu.Unlock()

	if c.key != nil {
		return c.key, nil
	}

	if c.secret == nil {
		return nil, errors.New("missing cluster secret")
	}

	secret, err := c.secret()
	if err != nil {
		return nil, err
	}
	if secret == "" {
		return nil, ErrNotReady
	}

	c.key = []byte(secret)

	return c.key, nil
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// channelName returns the cluster channel name derived from the secret key.
func channelName(key []byte) string {
	return ChannelPrefix + hex.EncodeToString(mac(key, "channel"))[:20]
}

// sign returns the signature of the message data (nodeId|topic|payload).
func sign(key []byte, data string) string {
	return base64.RawURLEncoding.EncodeToString(mac(key, "message|"+data))[:signatureLength]
}

func parseMessage(key []byte, raw string) (Message, bool) {
	signature, data, ok := strings.Cut(raw, "|")
	if !ok || !hmac.Equal([]byte(signature), []byte(sign(key, data))) {
		return Message{}, false
	}

	parts := strings.SplitN(data, "|", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return Message{}, false
	}

	return Message{NodeId: parts[0], Topic: parts[1], Payload: parts[2]}, true
}
