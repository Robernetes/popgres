package migrations

import (
	"github.com/Robernetes/popgres/core"
)

func init() {
	core.SystemMigrations.Add(&core.Migration{
		Up: func(txApp core.App) error {
			// note: the aux connections use search_path=_aux (see BaseApp.initAuxDB)
			_, execErr := txApp.AuxDB().NewQuery(`
				CREATE SCHEMA IF NOT EXISTS {{_aux}};

				CREATE TABLE IF NOT EXISTS {{_logs}} (
					[[id]]      TEXT PRIMARY KEY DEFAULT ('r' || substr(md5(random()::text || clock_timestamp()::text), 1, 14)) NOT NULL,
					[[level]]   INTEGER DEFAULT 0 NOT NULL,
					[[message]] TEXT DEFAULT '' NOT NULL,
					[[data]]    JSONB DEFAULT '{}' NOT NULL,
					[[created]] TIMESTAMPTZ DEFAULT now() NOT NULL
				);

				CREATE INDEX IF NOT EXISTS idx_logs_level ON {{_logs}} ([[level]]);
				CREATE INDEX IF NOT EXISTS idx_logs_message ON {{_logs}} ([[message]]);
				CREATE INDEX IF NOT EXISTS idx_logs_created_hour ON {{_logs}} (date_trunc('hour', [[created]] AT TIME ZONE 'UTC'));

				-- the json helpers are created in the _aux schema too because
				-- the logs could be stored in a separate database
			` + popgresJSONFunctions + popgresStrftimeFunctions).Execute()

			return execErr
		},
		Down: func(txApp core.App) error {
			_, err := txApp.AuxDB().DropTable("_logs").Execute()
			return err
		},
		ReapplyCondition: func(txApp core.App, runner *core.MigrationsRunner, fileName string) (bool, error) {
			// reapply only if the _logs table doesn't exist
			exists := txApp.AuxHasTable("_logs")
			return !exists, nil
		},
	})
}
