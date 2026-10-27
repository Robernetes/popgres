package core

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/pocketbase/dbx"
)

// RealtimeSourceEnv is the env variable used as fallback for BaseAppConfig.RealtimeSource.
const RealtimeSourceEnv = "POPGRES_REALTIME_SOURCE"

// initRealtimeSource resolves the realtime events source of the app instance
// and starts the replication (if enabled).
//
// The "replication" source falls back to "hooks" (with a warning) if the
// Postgres logical replication is not available. The replication is checked
// again periodically and the node switches to it once it is available.
func (app *BaseApp) initRealtimeSource() {
	source := strings.ToLower(strings.TrimSpace(app.config.RealtimeSource))
	replication := false

	switch source {
	case cluster.RealtimeSourceReplication:
		replication = true

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := app.cluster.CheckReplicationSupport(ctx)
		cancel()

		if err != nil {
			app.Logger().Warn(
				"The replication realtime source is not available, the hooks source is used instead (checked again every minute)",
				slog.String("error", err.Error()),
			)
			source = cluster.RealtimeSourceHooks
		}
	case "", cluster.RealtimeSourceHooks:
		source = cluster.RealtimeSourceHooks
	default:
		app.Logger().Warn(
			"Unknown realtime source, the hooks source is used instead",
			slog.String("source", source),
		)
		source = cluster.RealtimeSourceHooks
	}

	app.cluster.SetRealtimeSource(source)

	if replication {
		app.cluster.StartReplication(cluster.ReplicationConfig{
			PrepareTables:     app.prepareRealtimePublication,
			ResolveCollection: app.resolveRealtimeCollection,
		})
	}
}

func (app *BaseApp) resolveRealtimeCollection(table string) string {
	collection, err := app.FindCachedCollectionByNameOrId(table)
	if err != nil {
		return table
	}

	return collection.Id
}

// prepareRealtimePublication ensures that the realtime publication contains
// all collection tables and that they have REPLICA IDENTITY FULL
// (so that the deleted records snapshot has all columns).
func (app *BaseApp) prepareRealtimePublication() error {
	collections, err := app.FindAllCollections(CollectionTypeBase, CollectionTypeAuth)
	if err != nil {
		return err
	}

	tables := make([]string, 0, len(collections))
	for _, c := range collections {
		tables = append(tables, "{{"+c.Name+"}}")
	}

	return app.RunInTransaction(func(txApp App) error {
		var exists bool
		err := txApp.DB().NewQuery("SELECT EXISTS (SELECT 1 FROM pg_publication WHERE [[pubname]] = {:name})").
			Bind(dbx.Params{"name": cluster.PublicationName}).
			Row(&exists)
		if err != nil {
			return err
		}

		action := "ALTER PUBLICATION {{" + cluster.PublicationName + "}} SET TABLE "
		if !exists {
			action = "CREATE PUBLICATION {{" + cluster.PublicationName + "}} FOR TABLE "
		}

		if _, err := txApp.DB().NewQuery(action + strings.Join(tables, ", ")).Execute(); err != nil {
			return err
		}

		for _, c := range collections {
			if err := setReplicaIdentityFull(txApp, c.Name); err != nil {
				return err
			}
		}

		return nil
	})
}

// addTableToRealtimePublication adds the collection table to the realtime
// publication if it exists (aka. the replication realtime source is used).
func addTableToRealtimePublication(txApp App, tableName string) error {
	var exists bool
	err := txApp.DB().NewQuery("SELECT EXISTS (SELECT 1 FROM pg_publication WHERE [[pubname]] = {:name})").
		Bind(dbx.Params{"name": cluster.PublicationName}).
		Row(&exists)
	if err != nil || !exists {
		return err
	}

	if err := setReplicaIdentityFull(txApp, tableName); err != nil {
		return err
	}

	_, err = txApp.DB().NewQuery("ALTER PUBLICATION {{" + cluster.PublicationName + "}} ADD TABLE {{" + tableName + "}}").Execute()

	return err
}

func setReplicaIdentityFull(txApp App, tableName string) error {
	var identity string
	err := txApp.DB().NewQuery("SELECT [[relreplident]]::text FROM pg_class WHERE [[oid]] = to_regclass({:table})").
		Bind(dbx.Params{"table": txApp.DB().QuoteSimpleTableName(tableName)}).
		Row(&identity)
	if err != nil {
		return err
	}

	if identity == "f" {
		return nil
	}

	_, err = txApp.DB().NewQuery("ALTER TABLE {{" + tableName + "}} REPLICA IDENTITY FULL").Execute()

	return err
}
