package migrations

import (
	"github.com/Robernetes/popgres/core"
)

// popgres cluster tables.
//
// They store the state shared by the app instances (nodes) of the same
// database. The tables are UNLOGGED because their data is temporary and
// it is acceptable to lose it if the Postgres server crashes.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		_, err := txApp.DB().NewQuery(`
			-- the keys with expiration (eg. the resend timeouts of the auth emails)
			CREATE UNLOGGED TABLE IF NOT EXISTS {{_kv_ttl}} (
				[[key]]        TEXT PRIMARY KEY NOT NULL,
				[[value]]      TEXT DEFAULT '' NOT NULL,
				[[expires_at]] TIMESTAMPTZ NOT NULL
			);
			CREATE INDEX IF NOT EXISTS {{idx__kv_ttl_expires_at}} ON {{_kv_ttl}} ([[expires_at]]);

			-- the cron job executions claimed by the nodes (one per job and scheduled minute)
			CREATE UNLOGGED TABLE IF NOT EXISTS {{_cron_runs}} (
				[[job_id]]  TEXT NOT NULL,
				[[slot]]    TIMESTAMPTZ NOT NULL,
				[[node_id]] TEXT NOT NULL,
				[[created]] TIMESTAMPTZ DEFAULT now() NOT NULL,
				PRIMARY KEY ([[job_id]], [[slot]])
			);
			CREATE INDEX IF NOT EXISTS {{idx__cron_runs_created}} ON {{_cron_runs}} ([[created]]);

			-- the fixed window rate limit counters
			CREATE UNLOGGED TABLE IF NOT EXISTS {{_rate_limits}} (
				[[key]]          TEXT PRIMARY KEY NOT NULL,
				[[window_start]] TIMESTAMPTZ NOT NULL,
				[[hits]]         INTEGER DEFAULT 0 NOT NULL,
				[[expires_at]]   TIMESTAMPTZ NOT NULL
			);
			CREATE INDEX IF NOT EXISTS {{idx__rate_limits_expires_at}} ON {{_rate_limits}} ([[expires_at]]);

			-- the realtime events published to the other nodes
			-- (the records snapshot is needed for the deleted records)
			CREATE UNLOGGED TABLE IF NOT EXISTS {{_realtime_events}} (
				[[id]]            TEXT PRIMARY KEY NOT NULL,
				[[source]]        TEXT NOT NULL,
				[[op]]            TEXT NOT NULL,
				[[collection_id]] TEXT NOT NULL,
				[[record_id]]     TEXT NOT NULL,
				[[snapshot]]      JSONB,
				[[created]]       TIMESTAMPTZ DEFAULT now() NOT NULL
			);
			CREATE INDEX IF NOT EXISTS {{idx__realtime_events_created}} ON {{_realtime_events}} ([[created]]);
		`).Execute()

		return err
	}, func(txApp core.App) error {
		_, err := txApp.DB().NewQuery(`
			DROP TABLE IF EXISTS {{_realtime_events}};
			DROP TABLE IF EXISTS {{_rate_limits}};
			DROP TABLE IF EXISTS {{_cron_runs}};
			DROP TABLE IF EXISTS {{_kv_ttl}};
		`).Execute()

		return err
	})
}
