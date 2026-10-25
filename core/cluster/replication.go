package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/pocketbase/dbx"
)

// Realtime events from the Postgres logical replication.
//
// One node (the leader, elected with a session advisory lock) consumes the
// logical replication slot of the database (pgoutput) and for each committed
// change of a collection table it stores an event in the _realtime_events table
// and publishes the event ids to all nodes. Each node then broadcasts the
// events to its own realtime clients.
//
// The event id is derived from the transaction commit LSN and the change index,
// and the events are inserted with ON CONFLICT DO NOTHING, so decoding the same
// transaction again (eg. after a leader change) doesn't create new events.
// The slot position is confirmed only after the events are stored
// ("at least once" delivery; the nodes discard the already received event ids).

// The realtime event sources.
const (
	RealtimeSourceHooks       = "hooks"
	RealtimeSourceReplication = "replication"
)

const (
	// PublicationName is the Postgres publication with the collection tables.
	PublicationName = "popgres_rt_pub"

	// TopicRealtimeEvents is the cluster topic with the ids of the stored realtime events
	// (comma separated).
	TopicRealtimeEvents = "rt_events"

	// RealtimeEventsTable is the UNLOGGED table of the realtime events (created by the system migrations).
	RealtimeEventsTable = "_realtime_events"

	replicationLeaderLock = "realtime_replication_leader"

	// slotNamePrefix is the prefix of the logical replication slot name
	// (the slots are per server, so the name includes the database name).
	slotNamePrefix = "popgres_rt_"

	maxSlotNameLength = 63

	leaderRetryInterval    = 2 * time.Second
	standbyStatusInterval  = 10 * time.Second
	maxEventIdsPerNotify   = 150
	slotMetricsLogInterval = time.Minute

	// RetainedWALWarningSize is the retained WAL size (in bytes) of the
	// realtime slot above which a warning is logged.
	RetainedWALWarningSize = 1 << 30
)

var slotNameInvalidChars = regexp.MustCompile(`[^a-z0-9_]+`)

// SlotName returns the name of the realtime logical replication slot of the database.
//
// The name is "popgres_rt_<database>" (with the invalid characters replaced) and it is
// truncated with a hash suffix if it is longer than 63 characters.
func SlotName(database string) string {
	name := slotNamePrefix + slotNameInvalidChars.ReplaceAllString(strings.ToLower(database), "_")

	if len(name) <= maxSlotNameLength {
		return name
	}

	sum := sha256.Sum256([]byte(database))
	suffix := "_" + hex.EncodeToString(sum[:])[:8]

	return name[:maxSlotNameLength-len(suffix)] + suffix
}

// ReplicationConfig defines the callbacks used by the replication leader.
type ReplicationConfig struct {
	// PrepareTables ensures that the publication contains all collection
	// tables (with REPLICA IDENTITY FULL). It is called by the leader before
	// consuming the slot.
	PrepareTables func() error

	// ResolveCollection returns the collection id of the specified table
	// (or the table name if it can't be resolved).
	ResolveCollection func(table string) string
}

// RealtimeEvent is a stored realtime event.
type RealtimeEvent struct {
	Id           string `db:"id"`
	Source       string `db:"source"`
	Op           string `db:"op"`
	CollectionId string `db:"collection_id"`
	RecordId     string `db:"record_id"`
	Snapshot     string `db:"snapshot"` // json, only for the deleted records
}

type replicationState struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// SetRealtimeSource sets the realtime source of the current node (see [RealtimeSourceHooks]).
func (c *Cluster) SetRealtimeSource(source string) {
	c.realtimeSource.Store(source)
}

// RealtimeSource returns the realtime source of the current node
// ([RealtimeSourceHooks] if not set).
func (c *Cluster) RealtimeSource() string {
	source, _ := c.realtimeSource.Load().(string)
	if source == "" {
		return RealtimeSourceHooks
	}
	return source
}

// IsReplicationLeader reports whether the current node consumes the replication slot.
func (c *Cluster) IsReplicationLeader() bool {
	return c.replicationLeader.Load()
}

// CheckReplicationSupport reports whether the logical replication can be used
// for the realtime events (wal_level=logical and a role with the REPLICATION attribute).
func (c *Cluster) CheckReplicationSupport(ctx context.Context) error {
	db, err := c.pool()
	if err != nil {
		return err
	}

	var walLevel string
	if err := db.NewQuery("SHOW wal_level").WithContext(ctx).Row(&walLevel); err != nil {
		return err
	}
	if walLevel != "logical" {
		return fmt.Errorf("the server wal_level is %q (logical is required)", walLevel)
	}

	var canReplicate bool
	err = db.NewQuery("SELECT [[rolreplication]] OR [[rolsuper]] FROM pg_roles WHERE [[rolname]] = current_user").
		WithContext(ctx).
		Row(&canReplicate)
	if err != nil {
		return err
	}
	if !canReplicate {
		return errors.New("the database role doesn't have the REPLICATION attribute")
	}

	return nil
}

// StartReplication starts competing for the replication leadership in the background.
//
// It is no-op if it is already started.
func (c *Cluster) StartReplication(config ReplicationConfig) {
	c.replicationMu.Lock()
	defer c.replicationMu.Unlock()

	if c.replication != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	state := &replicationState{cancel: cancel, done: make(chan struct{})}
	c.replication = state

	go func() {
		defer close(state.done)
		c.runReplication(ctx, config)
	}()
}

// StopReplication stops the replication (releasing the leadership) and waits for it to finish.
func (c *Cluster) StopReplication() {
	c.replicationMu.Lock()
	state := c.replication
	c.replication = nil
	c.replicationMu.Unlock()

	if state == nil {
		return
	}

	state.cancel()
	<-state.done
}

func (c *Cluster) runReplication(ctx context.Context, config ReplicationConfig) {
	delay := minReconnectDelay

	for ctx.Err() == nil {
		isLeader, err := c.leadReplication(ctx, config)
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			c.logger().Warn(
				"Realtime replication failure, retrying...",
				slog.String("error", err.Error()),
				slog.String("retryIn", delay.String()),
			)
			sleepCtx(ctx, delay)
			delay = min(delay*2, maxReconnectDelay)
			continue
		}

		if !isLeader {
			delay = minReconnectDelay
			sleepCtx(ctx, leaderRetryInterval)
		}
	}
}

// leadReplication tries to become the replication leader and consumes the slot
// until an error or the context cancellation.
func (c *Cluster) leadReplication(ctx context.Context, config ReplicationConfig) (bool, error) {
	dsn := c.dsn()

	// the session level leader lock is held by a dedicated connection
	// and it is released when the connection is closed (eg. on a crash)
	lockConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return false, err
	}
	defer closePgx(lockConn)

	var locked bool
	if err := lockConn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", LockKey(replicationLeaderLock)).Scan(&locked); err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}

	c.replicationLeader.Store(true)
	defer c.replicationLeader.Store(false)

	c.logger().Debug("Realtime replication leadership acquired", slog.String("nodeId", c.nodeId))

	return true, c.consume(ctx, config, lockConn)
}

func (c *Cluster) consume(ctx context.Context, config ReplicationConfig, lockConn *pgx.Conn) error {
	db, err := c.pool()
	if err != nil {
		return err
	}

	if config.PrepareTables != nil {
		if err := config.PrepareTables(); err != nil {
			return fmt.Errorf("failed to prepare the publication tables: %w", err)
		}
	}

	var database string
	if err := db.NewQuery("SELECT current_database()").WithContext(ctx).Row(&database); err != nil {
		return err
	}
	slot := SlotName(database)

	replDSN, err := replicationDSN(c.dsn())
	if err != nil {
		return err
	}

	conn, err := pgconn.Connect(ctx, replDSN)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if err := c.ensureSlot(ctx, db, conn, slot); err != nil {
		return err
	}

	err = pglogrepl.StartReplication(ctx, conn, slot, 0, pglogrepl.StartReplicationOptions{
		PluginArgs: []string{
			"proto_version '1'",
			"publication_names '" + PublicationName + "'",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to start the replication of slot %s: %w", slot, err)
	}

	decoder := &replicationDecoder{
		relations:         map[uint32]*pglogrepl.RelationMessage{},
		resolveCollection: config.ResolveCollection,
	}

	var confirmed pglogrepl.LSN
	nextStatus := time.Now().Add(standbyStatusInterval)
	nextMetrics := time.Now()

	sendStatus := func() error {
		nextStatus = time.Now().Add(standbyStatusInterval)
		if confirmed == 0 {
			return nil
		}
		return pglogrepl.SendStandbyStatusUpdate(ctx, conn, pglogrepl.StandbyStatusUpdate{WALWritePosition: confirmed})
	}

	for {
		if time.Now().After(nextStatus) {
			if err := sendStatus(); err != nil {
				return err
			}

			// the leader lock connection is checked periodically
			if err := lockConn.Ping(ctx); err != nil {
				return fmt.Errorf("lost the leader lock connection: %w", err)
			}
		}

		if time.Now().After(nextMetrics) {
			nextMetrics = time.Now().Add(slotMetricsLogInterval)
			c.checkSlotMetrics(ctx, slot)
		}

		receiveCtx, cancel := context.WithDeadline(ctx, nextStatus)
		rawMsg, err := conn.ReceiveMessage(receiveCtx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) && ctx.Err() == nil {
				continue
			}
			return err
		}

		if errMsg, ok := rawMsg.(*pgproto3.ErrorResponse); ok {
			return fmt.Errorf("replication error: %s (%s)", errMsg.Message, errMsg.Code)
		}

		msg, ok := rawMsg.(*pgproto3.CopyData)
		if !ok || len(msg.Data) == 0 {
			continue
		}

		switch msg.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
			if err != nil {
				return err
			}

			// outside of a transaction the WAL up to the server end has nothing to stream
			// (it is confirmed so that the slot doesn't retain it)
			if !decoder.inTransaction && pkm.ServerWALEnd > confirmed {
				confirmed = pkm.ServerWALEnd
			}

			if pkm.ReplyRequested {
				if err := sendStatus(); err != nil {
					return err
				}
			}
		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				return err
			}

			committed, endLSN, err := decoder.decode(xld.WALData)
			if err != nil {
				return err
			}

			if committed != nil {
				if err := c.storeAndPublishEvents(ctx, db, committed); err != nil {
					return err
				}

				confirmed = endLSN
				if err := sendStatus(); err != nil {
					return err
				}
			}
		}
	}
}

// ensureSlot creates the logical replication slot if missing
// (or recreates it if it was invalidated).
//
// The slot is created with the replication protocol command (replConn) instead of
// pg_create_logical_replication_slot() because the SQL function runs in a transaction
// and the concurrent slot creations (eg. of other databases on the same server)
// would wait for each other's transaction to reach a consistent point.
func (c *Cluster) ensureSlot(ctx context.Context, db *dbx.DB, replConn *pgconn.PgConn, slot string) error {
	var walStatus string

	err := db.NewQuery("SELECT COALESCE([[wal_status]], '') FROM pg_replication_slots WHERE [[slot_name]] = {:slot}").
		WithContext(ctx).
		Bind(dbx.Params{"slot": slot}).
		Row(&walStatus)

	exists := err == nil
	if err != nil && !isNoRows(err) {
		return err
	}

	if exists && walStatus == "lost" {
		c.logger().Warn(
			"The realtime replication slot was invalidated (the realtime events of the changes since then are lost), recreating it",
			slog.String("slot", slot),
		)

		_, err := db.NewQuery("SELECT pg_drop_replication_slot({:slot})").
			WithContext(ctx).
			Bind(dbx.Params{"slot": slot}).
			Execute()
		if err != nil {
			return fmt.Errorf("failed to drop the invalidated slot %s: %w", slot, err)
		}

		exists = false
	}

	if !exists {
		_, err := pglogrepl.CreateReplicationSlot(ctx, replConn, slot, "pgoutput", pglogrepl.CreateReplicationSlotOptions{
			Mode:           pglogrepl.LogicalReplication,
			SnapshotAction: "NOEXPORT_SNAPSHOT",
		})
		if err != nil {
			return fmt.Errorf("failed to create the replication slot %s: %w", slot, err)
		}
	}

	return nil
}

// SlotMetrics are the realtime replication slot metrics.
type SlotMetrics struct {
	Slot string `db:"slot_name" json:"slot"`

	// Active reports whether a node consumes the slot.
	Active bool `db:"active" json:"active"`

	// WALStatus is the Postgres wal_status of the slot (eg. "reserved" or "lost").
	WALStatus string `db:"wal_status" json:"walStatus"`

	// LagBytes is the size of the WAL that is not confirmed by the consumer yet.
	LagBytes int64 `db:"lag_bytes" json:"lagBytes"`

	// RetainedBytes is the size of the WAL retained by the slot.
	RetainedBytes int64 `db:"retained_bytes" json:"retainedBytes"`
}

// SlotMetrics returns the realtime replication slot metrics of the database
// (or nil if there is no slot).
func (c *Cluster) SlotMetrics(ctx context.Context) (*SlotMetrics, error) {
	db, err := c.pool()
	if err != nil {
		return nil, err
	}

	metrics := &SlotMetrics{}

	err = db.NewQuery(`
		SELECT
			[[slot_name]],
			[[active]],
			COALESCE([[wal_status]], '') AS [[wal_status]],
			COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), [[confirmed_flush_lsn]]), 0)::bigint AS [[lag_bytes]],
			COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), [[restart_lsn]]), 0)::bigint AS [[retained_bytes]]
		FROM pg_replication_slots
		WHERE [[database]] = current_database() AND starts_with([[slot_name]], {:prefix})
		LIMIT 1
	`).WithContext(ctx).Bind(dbx.Params{"prefix": slotNamePrefix}).One(metrics)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return metrics, nil
}

func (c *Cluster) checkSlotMetrics(ctx context.Context, slot string) {
	metrics, err := c.SlotMetrics(ctx)
	if err != nil || metrics == nil {
		return
	}

	if metrics.RetainedBytes > RetainedWALWarningSize {
		c.logger().Warn(
			"The realtime replication slot retains a large amount of WAL",
			slog.String("slot", slot),
			slog.Int64("retainedBytes", metrics.RetainedBytes),
			slog.Int64("lagBytes", metrics.LagBytes),
		)
	}
}

// storeAndPublishEvents stores the events (ignoring the already stored ones)
// and publishes their ids to all nodes.
func (c *Cluster) storeAndPublishEvents(ctx context.Context, db *dbx.DB, events []*RealtimeEvent) error {
	if len(events) == 0 {
		return nil
	}

	for start := 0; start < len(events); start += maxEventIdsPerNotify {
		batch := events[start:min(start+maxEventIdsPerNotify, len(events))]

		params := dbx.Params{}
		values := make([]string, 0, len(batch))
		ids := make([]string, 0, len(batch))

		for i, e := range batch {
			p := fmt.Sprintf("e%d_", i)
			params[p+"id"] = e.Id
			params[p+"op"] = e.Op
			params[p+"collection"] = e.CollectionId
			params[p+"record"] = e.RecordId
			if e.Snapshot != "" {
				params[p+"snapshot"] = e.Snapshot
			} else {
				params[p+"snapshot"] = nil
			}

			values = append(values, fmt.Sprintf(
				"({:%sid}, '%s', {:%sop}, {:%scollection}, {:%srecord}, {:%ssnapshot}::jsonb)",
				p, RealtimeSourceReplication, p, p, p, p,
			))
			ids = append(ids, e.Id)
		}

		// note: the already stored events (eg. decoded again after a leader change)
		// are still published because the previous leader could have stopped before publishing them
		_, err := db.NewQuery(
			"INSERT INTO {{_realtime_events}} ([[id]], [[source]], [[op]], [[collection_id]], [[record_id]], [[snapshot]]) VALUES " +
				strings.Join(values, ",") +
				" ON CONFLICT DO NOTHING",
		).WithContext(ctx).Bind(params).Execute()
		if err != nil {
			return fmt.Errorf("failed to store the realtime events: %w", err)
		}

		if err := c.Publish(db, TopicRealtimeEvents, strings.Join(ids, ",")); err != nil {
			return fmt.Errorf("failed to publish the realtime events: %w", err)
		}
	}

	return nil
}

// FindRealtimeEvents returns the stored realtime events with the specified ids (ordered by id).
func (c *Cluster) FindRealtimeEvents(ids []string) ([]*RealtimeEvent, error) {
	db, err := c.pool()
	if err != nil {
		return nil, err
	}

	values := make([]any, len(ids))
	for i, id := range ids {
		values[i] = id
	}

	events := []*RealtimeEvent{}

	err = db.Select("id", "source", "op", "collection_id", "record_id", "COALESCE(snapshot::text, '') AS snapshot").
		From(RealtimeEventsTable).
		Where(dbx.In("id", values...)).
		OrderBy("id ASC").
		All(&events)

	return events, err
}

// -------------------------------------------------------------------

// replicationDecoder converts the pgoutput messages into realtime events.
type replicationDecoder struct {
	relations         map[uint32]*pglogrepl.RelationMessage
	resolveCollection func(table string) string

	inTransaction bool
	finalLSN      pglogrepl.LSN
	index         int
	events        []*RealtimeEvent
}

// decode handles a single pgoutput message and returns the events
// of the transaction and its end LSN when the message is a commit.
func (d *replicationDecoder) decode(data []byte) ([]*RealtimeEvent, pglogrepl.LSN, error) {
	msg, err := pglogrepl.Parse(data)
	if err != nil {
		return nil, 0, err
	}

	switch m := msg.(type) {
	case *pglogrepl.RelationMessage:
		d.relations[m.RelationID] = m
	case *pglogrepl.BeginMessage:
		d.inTransaction = true
		d.finalLSN = m.FinalLSN
		d.index = 0
		d.events = nil
	case *pglogrepl.InsertMessage:
		d.add("create", m.RelationID, m.Tuple, false)
	case *pglogrepl.UpdateMessage:
		d.add("update", m.RelationID, m.NewTuple, false)
	case *pglogrepl.DeleteMessage:
		d.add("delete", m.RelationID, m.OldTuple, true)
	case *pglogrepl.CommitMessage:
		events := d.events
		if events == nil {
			events = []*RealtimeEvent{}
		}
		d.inTransaction = false
		d.events = nil
		return events, m.TransactionEndLSN, nil
	}

	return nil, 0, nil
}

func (d *replicationDecoder) add(op string, relationId uint32, tuple *pglogrepl.TupleData, withSnapshot bool) {
	rel, ok := d.relations[relationId]
	if !ok || tuple == nil {
		return
	}

	// note: the index is incremented for every change so that the ids are stable
	d.index++
	eventId := fmt.Sprintf("%016X_%06d", uint64(d.finalLSN), d.index)

	var recordId string
	var snapshot jsontext.Value

	if withSnapshot {
		snapshot = tupleSnapshot(rel, tuple)
	}

	for i, col := range rel.Columns {
		if col.Name == "id" && i < len(tuple.Columns) && tuple.Columns[i].DataType == pglogrepl.TupleDataTypeText {
			recordId = string(tuple.Columns[i].Data)
			break
		}
	}
	if recordId == "" {
		return
	}

	collectionId := rel.RelationName
	if d.resolveCollection != nil {
		collectionId = d.resolveCollection(rel.RelationName)
	}

	d.events = append(d.events, &RealtimeEvent{
		Id:           eventId,
		Source:       RealtimeSourceReplication,
		Op:           op,
		CollectionId: collectionId,
		RecordId:     recordId,
		Snapshot:     string(snapshot),
	})
}

const (
	jsonOID  = 114
	jsonbOID = 3802
)

// tupleSnapshot returns the json object of the tuple columns
// (in the format expected by jsonb_populate_record).
func tupleSnapshot(rel *pglogrepl.RelationMessage, tuple *pglogrepl.TupleData) jsontext.Value {
	buf := &bytes.Buffer{}
	enc := jsontext.NewEncoder(buf)

	_ = enc.WriteToken(jsontext.BeginObject)

	for i, col := range rel.Columns {
		if i >= len(tuple.Columns) {
			break
		}

		value := tuple.Columns[i]
		if value.DataType == pglogrepl.TupleDataTypeToast {
			continue // unchanged toasted value (not available)
		}

		_ = enc.WriteToken(jsontext.String(col.Name))

		switch {
		case value.DataType == pglogrepl.TupleDataTypeNull:
			_ = enc.WriteToken(jsontext.Null)
		case (col.DataType == jsonOID || col.DataType == jsonbOID) && jsontext.Value(value.Data).IsValid():
			_ = enc.WriteValue(jsontext.Value(value.Data))
		default:
			_ = enc.WriteToken(jsontext.String(string(value.Data)))
		}
	}

	_ = enc.WriteToken(jsontext.EndObject)

	return jsontext.Value(strings.TrimSpace(buf.String()))
}

// -------------------------------------------------------------------

// replicationDSN returns the connection string with the logical replication protocol enabled.
func replicationDSN(dsn string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}

		q := u.Query()
		q.Set("replication", "database")
		u.RawQuery = q.Encode()

		return u.String(), nil
	}

	return dsn + " replication=database", nil
}

func closePgx(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = conn.Close(ctx)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows)
}

// -------------------------------------------------------------------

// EventsDeduplicator remembers the recently received event ids so that the
// events delivered more than once (eg. after a leader change) are discarded.
type EventsDeduplicator struct {
	seen  map[string]struct{}
	order []string
	max   int
	mu    sync.Mutex
}

// NewEventsDeduplicator creates a new deduplicator that remembers the last max ids.
func NewEventsDeduplicator(max int) *EventsDeduplicator {
	return &EventsDeduplicator{seen: map[string]struct{}{}, max: max}
}

// Filter returns the ids that were not seen before (sorted) and remembers them.
func (d *EventsDeduplicator) Filter(ids []string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	result := make([]string, 0, len(ids))

	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := d.seen[id]; ok {
			continue
		}

		d.seen[id] = struct{}{}
		d.order = append(d.order, id)
		result = append(result, id)
	}

	for len(d.order) > d.max {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}

	sort.Strings(result)

	return result
}
