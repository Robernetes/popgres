package apis

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tools/hook"
	"github.com/Robernetes/popgres/tools/inflector"
	"github.com/Robernetes/popgres/tools/search"
	"github.com/Robernetes/popgres/tools/security"
	"github.com/pocketbase/dbx"
)

// Realtime events between the app instances (nodes) that share the same database
// (the "hooks" realtime source).
//
// The node that changes a record broadcasts the event to its own clients
// (see bindRealtimeEvents) and in cluster mode it also publishes a cluster message:
//   - for a created or updated record the message has only the record ids
//     and the other nodes load the current record from the database;
//   - for a deleted record the node stores a snapshot of the deleted row in the
//     _realtime_events table (taken inside the delete transaction) and the message
//     has only the event id. The other nodes load the record from the snapshot and
//     check the API rules against it (with a CTE that replaces the collection table).
//
// Limitation: the access check of a deleted record that relies on a parent record
// deleted in the same transaction (eg. a cascade delete) fails on the other nodes.

const (
	clusterTopicRealtimeRecord         = "rt_record"
	clusterTopicRealtimeAuthCollection = "rt_auth_collection"

	realtimeEventsTable = "_realtime_events"
	realtimeSourceHooks = "hooks"
)

// bindRealtimeClusterEvents registers the cluster realtime event hooks and handlers.
func bindRealtimeClusterEvents(app core.App) {
	// the deleted records snapshots (dry cache key -> snapshot)
	snapshots := &sync.Map{}

	// receive
	// ---------------------------------------------------------------

	app.Cluster().Subscribe(clusterTopicRealtimeRecord, func(msg cluster.Message) {
		if msg.NodeId == app.Cluster().NodeId() {
			return // already broadcasted locally
		}

		if err := realtimeHandleClusterRecordMessage(app, msg.Payload); err != nil {
			app.Logger().Debug(
				"Failed to broadcast the realtime record event of another node",
				slog.String("payload", msg.Payload),
				slog.String("error", err.Error()),
			)
		}
	})

	app.Cluster().Subscribe(clusterTopicRealtimeAuthCollection, func(msg cluster.Message) {
		if msg.NodeId == app.Cluster().NodeId() {
			return // already handled locally
		}

		// payload: collectionName
		err := realtimeUnsetClientsAuthByCollection(app, core.NewAuthCollection(msg.Payload))
		if err != nil {
			app.Logger().Warn(
				"Failed to remove client(s) associated to the changed auth collection of another node",
				slog.String("collectionName", msg.Payload),
				slog.String("error", err.Error()),
			)
		}
	})

	// the replication events (published by the replication leader, eg. the current node)
	dedup := cluster.NewEventsDeduplicator(10000)
	app.Cluster().Subscribe(cluster.TopicRealtimeEvents, func(msg cluster.Message) {
		if !realtimeFromReplication(app) {
			return
		}

		ids := dedup.Filter(strings.Split(msg.Payload, ","))
		if len(ids) == 0 {
			return
		}

		if err := realtimeBroadcastReplicationEvents(app, ids); err != nil {
			app.Logger().Debug(
				"Failed to broadcast the realtime replication events",
				slog.String("error", err.Error()),
			)
		}
	})

	// publish
	// ---------------------------------------------------------------

	notify := func(topic string, payload string) {
		if err := app.Cluster().Notify(topic, payload); err != nil {
			app.Logger().Warn(
				"Failed to publish the realtime cluster event",
				slog.String("topic", topic),
				slog.String("error", err.Error()),
			)
		}
	}

	// the auth state of the clients of all nodes is reset on auth collection delete
	// (always published since it is rare and security related; for the secret
	// change see realtimeNotifyAuthCollectionChange)
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*core.CollectionEvent]{
		Func: func(e *core.CollectionEvent) error {
			if e.Collection.IsAuth() {
				notify(clusterTopicRealtimeAuthCollection, e.Collection.Name)
			}
			return e.Next()
		},
		Priority: -99,
	})

	app.OnModelAfterCreateSuccess().Bind(&hook.Handler[*core.ModelEvent]{
		Func: func(e *core.ModelEvent) error {
			if app.Cluster().Enabled() && !realtimeFromReplication(app) {
				if collection := realtimeResolveRecordCollection(e.App, e.Model); collection != nil {
					notify(clusterTopicRealtimeRecord, realtimeRecordPayload("create", collection, e.Model.PK()))
				}
			}
			return e.Next()
		},
		Priority: -99,
	})

	app.OnModelAfterUpdateSuccess().Bind(&hook.Handler[*core.ModelEvent]{
		Func: func(e *core.ModelEvent) error {
			if app.Cluster().Enabled() && !realtimeFromReplication(app) {
				if collection := realtimeResolveRecordCollection(e.App, e.Model); collection != nil {
					notify(clusterTopicRealtimeRecord, realtimeRecordPayload("update", collection, e.Model.PK()))
				}
			}
			return e.Next()
		},
		Priority: -99,
	})

	// delete: snapshot the record row before the delete (inside the delete transaction)
	app.OnModelDelete().Bind(&hook.Handler[*core.ModelEvent]{
		Func: func(e *core.ModelEvent) error {
			if app.Cluster().Enabled() && !realtimeFromReplication(app) {
				if collection := realtimeResolveRecordCollection(e.App, e.Model); collection != nil {
					snapshot, err := realtimeRecordSnapshot(e.App, collection, e.Model.PK())
					if err != nil {
						app.Logger().Debug(
							"Failed to snapshot the deleted record for the cluster realtime event",
							slog.Any("id", e.Model.PK()),
							slog.String("collectionName", collection.Name),
							slog.String("error", err.Error()),
						)
					} else {
						snapshots.Store(getDryCacheKey("delete", e.Model), snapshot)
					}
				}
			}

			return e.Next()
		},
		Priority: 99, // execute as later as possible
	})

	// delete: store the snapshot and publish the event id
	app.OnModelAfterDeleteSuccess().Bind(&hook.Handler[*core.ModelEvent]{
		Func: func(e *core.ModelEvent) error {
			snapshot, ok := snapshots.LoadAndDelete(getDryCacheKey("delete", e.Model))
			if ok {
				collection := realtimeResolveRecordCollection(e.App, e.Model)
				if collection != nil {
					eventId, err := realtimeStoreDeleteEvent(app, collection.Id, e.Model.PK(), snapshot.(string))
					if err != nil {
						app.Logger().Warn(
							"Failed to store the cluster realtime delete event",
							slog.Any("id", e.Model.PK()),
							slog.String("collectionName", collection.Name),
							slog.String("error", err.Error()),
						)
					} else {
						notify(clusterTopicRealtimeRecord, "delete|"+eventId+"|"+collection.Name)
					}
				}
			}

			return e.Next()
		},
		Priority: -99,
	})

	app.OnModelAfterDeleteError().Bind(&hook.Handler[*core.ModelErrorEvent]{
		Func: func(e *core.ModelErrorEvent) error {
			snapshots.Delete(getDryCacheKey("delete", e.Model))
			return e.Next()
		},
		Priority: -99,
	})
}

// realtimeNotifyAuthCollectionChange notifies the other nodes (after the
// transaction commit, if any) to reset the auth state of their clients
// related to the auth collection (eg. on token secret change).
func realtimeNotifyAuthCollectionChange(app core.App, collectionName string) {
	execAfterSuccessTx(true, app, func() error {
		if err := app.Cluster().Notify(clusterTopicRealtimeAuthCollection, collectionName); err != nil {
			app.Logger().Warn(
				"Failed to publish the realtime auth collection change",
				slog.String("collectionName", collectionName),
				slog.String("error", err.Error()),
			)
		}
		return nil
	})
}

// realtimeRecordPayload returns the create/update cluster message payload
// (action|collectionId|collectionName|recordId).
func realtimeRecordPayload(action string, collection *core.Collection, pk any) string {
	return action + "|" + collection.Id + "|" + collection.Name + "|" + fmt.Sprint(pk)
}

// realtimeFindClusterCollection finds the collection of a cluster record event.
//
// The collection is searched also by name because the node that published the
// event could have used a stale cached collection (eg. a collection that was
// just deleted and created again with the same name by another node).
func realtimeFindClusterCollection(app core.App, collectionId string, collectionName string) (*core.Collection, error) {
	collection, err := app.FindCachedCollectionByNameOrId(collectionId)
	if err == nil {
		return collection, nil
	}

	return app.FindCachedCollectionByNameOrId(collectionName)
}

// realtimeRecordSnapshot returns the json representation of the stored record row.
func realtimeRecordSnapshot(app core.App, collection *core.Collection, pk any) (string, error) {
	var snapshot string

	err := app.DB().NewQuery(fmt.Sprintf(
		"SELECT to_jsonb([[t]])::text FROM {{%s}} [[t]] WHERE [[t.id]] = {:id}",
		inflector.Columnify(collection.Name),
	)).Bind(dbx.Params{"id": fmt.Sprint(pk)}).Row(&snapshot)

	return snapshot, err
}

// realtimeStoreDeleteEvent stores the deleted record snapshot and returns the new event id.
func realtimeStoreDeleteEvent(app core.App, collectionId string, pk any, snapshot string) (string, error) {
	eventId := security.RandomStringWithAlphabet(20, "abcdefghijklmnopqrstuvwxyz0123456789")

	// note: app is expected to be the main app instance and not the
	// (already committed) delete transaction app
	_, err := app.NonconcurrentDB().NewQuery(`
		INSERT INTO {{_realtime_events}} ([[id]], [[source]], [[op]], [[collection_id]], [[record_id]], [[snapshot]])
		VALUES ({:id}, {:source}, 'delete', {:collectionId}, {:recordId}, {:snapshot}::jsonb)
	`).Bind(dbx.Params{
		"id":           eventId,
		"source":       realtimeSourceHooks,
		"collectionId": collectionId,
		"recordId":     fmt.Sprint(pk),
		"snapshot":     snapshot,
	}).Execute()

	return eventId, err
}

// realtimeHandleClusterRecordMessage broadcasts a record event of another node to the local clients.
func realtimeHandleClusterRecordMessage(app core.App, payload string) error {
	// note: the record id is the last part because a custom id could contain "|"
	parts := strings.SplitN(payload, "|", 4)

	switch {
	case len(parts) == 4 && (parts[0] == "create" || parts[0] == "update"):
		action, collectionId, collectionName, recordId := parts[0], parts[1], parts[2], parts[3]

		collection, err := realtimeFindClusterCollection(app, collectionId, collectionName)
		if err != nil {
			return err
		}

		record, err := app.FindRecordById(collection, recordId)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // already deleted
			}
			return err
		}

		if action == "update" && collection.IsAuth() {
			if err := realtimeUpdateClientsAuth(app, record); err != nil {
				return err
			}
		}

		return realtimeBroadcastRecord(app, action, record, false)
	case len(parts) == 3 && parts[0] == "delete":
		// delete|eventId|collectionName
		return realtimeBroadcastClusterDeleteEvent(app, parts[1], parts[2])
	default:
		return errors.New("invalid realtime record message")
	}
}

func realtimeBroadcastClusterDeleteEvent(app core.App, eventId string, collectionName string) error {
	event := struct {
		CollectionId string `db:"collection_id"`
		Snapshot     string `db:"snapshot"`
	}{}

	err := app.ConcurrentDB().NewQuery(`
		SELECT [[collection_id]], [[snapshot]]::text AS [[snapshot]] FROM {{_realtime_events}}
		WHERE [[id]] = {:id}
	`).Bind(dbx.Params{"id": eventId}).One(&event)
	if err != nil {
		return err
	}

	collection, err := realtimeFindClusterCollection(app, event.CollectionId, collectionName)
	if err != nil {
		return err
	}

	return realtimeBroadcastDeletedSnapshot(app, collection, event.Snapshot)
}

// realtimeBroadcastDeletedSnapshot broadcasts the delete event of a record
// loaded from its snapshot (the API rules are checked against the snapshot).
func realtimeBroadcastDeletedSnapshot(app core.App, collection *core.Collection, snapshot string) error {
	withFrom, params := realtimeSnapshotCTE(collection.Name, collection.Name, snapshot)

	record := &core.Record{}
	err := app.RecordQuery(collection).PreFragment(withFrom).Bind(params).Limit(1).One(record)
	if err != nil {
		return err
	}

	if collection.IsAuth() {
		if err := realtimeUnsetClientsAuthByRecordModelOrProxy(app, record); err != nil {
			return err
		}
	}

	canAccess := func(record *core.Record, requestInfo *core.RequestInfo, rule *string) bool {
		return realtimeCanAccessRecordSnapshot(app, record, snapshot, requestInfo, rule)
	}

	return realtimeBroadcastRecordWithAccessCheck(app, "delete", record, false, canAccess)
}

// realtimeSnapshotCTE returns a "WITH" fragment that defines cteName
// as a single row of the collection table loaded from the json snapshot.
func realtimeSnapshotCTE(cteName string, collectionName string, snapshot string) (string, dbx.Params) {
	param := "__pb_rt_snapshot"

	// note: the NULL row of the table type is selected from the table itself instead of
	// casting to the type name (eg. NULL::posts) so that the cached prepared statement
	// depends on the table and it is invalidated if the collection table is recreated
	// (otherwise it fails with "cache lookup failed for type")
	withFrom := fmt.Sprintf(
		"WITH {{%s}} AS (SELECT (jsonb_populate_record((SELECT [[__pb_rt_row]] FROM {{%s}} [[__pb_rt_row]] WHERE FALSE), {:%s}::jsonb)).*)",
		inflector.Columnify(cteName),
		inflector.Columnify(collectionName),
		param,
	)

	return withFrom, dbx.Params{param: snapshot}
}

// realtimeCanAccessRecordSnapshot is similar to realtimeCanAccessRecord but the
// API rule and the client filter are checked against the record snapshot
// (eg. for an already deleted record).
func realtimeCanAccessRecordSnapshot(
	app core.App,
	record *core.Record,
	snapshot string,
	requestInfo *core.RequestInfo,
	accessRule *string,
) bool {
	if requestInfo.HasSuperuserAuth() {
		return realtimeSnapshotMatchesFilter(app, record, snapshot, requestInfo)
	}

	if accessRule == nil {
		return false
	}

	if *accessRule != "" && !realtimeSnapshotMatches(app, record, snapshot, requestInfo, *accessRule, true) {
		return false
	}

	return realtimeSnapshotMatchesFilter(app, record, snapshot, requestInfo)
}

// realtimeSnapshotMatchesFilter checks the subscription client-side filter (if any).
func realtimeSnapshotMatchesFilter(app core.App, record *core.Record, snapshot string, requestInfo *core.RequestInfo) bool {
	filter := requestInfo.Query[search.FilterQueryParam]
	if filter == "" {
		return true
	}

	if err := checkForSuperuserOnlyRuleFields(requestInfo); err != nil {
		return false
	}

	return realtimeSnapshotMatches(app, record, snapshot, requestInfo, filter, false)
}

func realtimeSnapshotMatches(
	app core.App,
	record *core.Record,
	snapshot string,
	requestInfo *core.RequestInfo,
	filter string,
	allowHiddenFields bool,
) bool {
	collection := record.Collection()

	// a different name so that the collection table can be still
	// referenced by the rule (eg. with @collection.*)
	dummyRandomPart := "__pb_rt__" + security.PseudorandomString(6)
	dummyCollection := *collection
	dummyCollection.Id += dummyRandomPart
	dummyCollection.Name += inflector.Columnify(dummyRandomPart)

	withFrom, params := realtimeSnapshotCTE(dummyCollection.Name, collection.Name, snapshot)

	query := app.ConcurrentDB().Select("(1)").PreFragment(withFrom).From(dummyCollection.Name).AndBind(params)

	resolver := core.NewRecordFieldResolver(app, &dummyCollection, requestInfo, allowHiddenFields)

	expr, err := search.FilterData(filter).BuildExpr(resolver)
	if err != nil {
		return false
	}
	query.AndWhere(expr)

	if err := resolver.UpdateQuery(query); err != nil {
		return false
	}

	var exists int
	err = query.Limit(1).Row(&exists)

	return err == nil && exists > 0
}

// realtimeFromReplication reports whether the realtime record events of the
// app instance come from the Postgres logical replication.
func realtimeFromReplication(app core.App) bool {
	return app.Cluster().RealtimeSource() == cluster.RealtimeSourceReplication
}

// realtimeBroadcastReplicationEvents broadcasts the stored replication events to the local clients.
func realtimeBroadcastReplicationEvents(app core.App, ids []string) error {
	events, err := app.Cluster().FindRealtimeEvents(ids)
	if err != nil {
		return err
	}

	var errs []error

	for _, event := range events {
		if err := realtimeBroadcastReplicationEvent(app, event); err != nil {
			errs = append(errs, fmt.Errorf("event %s: %w", event.Id, err))
		}
	}

	return errors.Join(errs...)
}

func realtimeBroadcastReplicationEvent(app core.App, event *cluster.RealtimeEvent) error {
	// note: the collection id could be the table name if it was not resolved by the leader
	collection, err := realtimeFindClusterCollection(app, event.CollectionId, event.CollectionId)
	if err != nil {
		return err
	}

	if event.Op == "delete" {
		return realtimeBroadcastDeletedSnapshot(app, collection, event.Snapshot)
	}

	record, err := app.FindRecordById(collection, event.RecordId)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already deleted
		}
		return err
	}

	if event.Op == "update" && collection.IsAuth() {
		if err := realtimeUpdateClientsAuth(app, record); err != nil {
			return err
		}
	}

	return realtimeBroadcastRecord(app, event.Op, record, false)
}
