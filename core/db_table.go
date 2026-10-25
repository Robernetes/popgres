package core

import (
	"database/sql"
	"fmt"

	"github.com/pocketbase/dbx"
)

// TableColumns returns all column names of a single table by its name.
func (app *BaseApp) TableColumns(tableName string) ([]string, error) {
	columns := []string{}

	err := app.ConcurrentDB().NewQuery(`
		SELECT [[attname]]
		FROM {{pg_attribute}}
		WHERE [[attrelid]] = to_regclass(quote_ident({:tableName}))
			AND [[attnum]] > 0
			AND NOT [[attisdropped]]
		ORDER BY [[attnum]]
	`).
		Bind(dbx.Params{"tableName": tableName}).
		Column(&columns)

	return columns, err
}

type TableInfoRow struct {
	// the `db:"pk"` tag has special semantic so we cannot rename
	// the original field without specifying a custom mapper
	PK int

	Index        int            `db:"cid"`
	Name         string         `db:"name"`
	Type         string         `db:"type"`
	NotNull      bool           `db:"notnull"`
	DefaultValue sql.NullString `db:"dflt_value"`
}

// TableInfo returns the columns info of the specified table
// in the same shape as the SQLite "table_info" pragma
// (cid, name, type, notnull, dflt_value and the 1-based primary key position).
func (app *BaseApp) TableInfo(tableName string) ([]*TableInfoRow, error) {
	info := []*TableInfoRow{}

	err := app.ConcurrentDB().NewQuery(`
		SELECT
			(a.[[attnum]] - 1) AS [[cid]],
			a.[[attname]] AS [[name]],
			format_type(a.[[atttypid]], a.[[atttypmod]]) AS [[type]],
			a.[[attnotnull]] AS [[notnull]],
			pg_get_expr(d.[[adbin]], d.[[adrelid]]) AS [[dflt_value]],
			COALESCE((
				SELECT k.[[n]]
				FROM {{pg_index}} i, unnest(i.[[indkey]]) WITH ORDINALITY AS k([[attnum]], [[n]])
				WHERE i.[[indrelid]] = a.[[attrelid]] AND i.[[indisprimary]] AND k.[[attnum]] = a.[[attnum]]
			), 0) AS [[pk]]
		FROM {{pg_attribute}} a
		LEFT JOIN {{pg_attrdef}} d ON d.[[adrelid]] = a.[[attrelid]] AND d.[[adnum]] = a.[[attnum]]
		WHERE a.[[attrelid]] = to_regclass(quote_ident({:tableName}))
			AND a.[[attnum]] > 0
			AND NOT a.[[attisdropped]]
		ORDER BY a.[[attnum]]
	`).
		Bind(dbx.Params{"tableName": tableName}).
		All(&info)
	if err != nil {
		return nil, err
	}

	// keep the SQLite behavior and return an error on missing table
	if len(info) == 0 {
		return nil, fmt.Errorf("empty table info probably due to invalid or missing table %s", tableName)
	}

	return info, nil
}

// TableIndexes returns a name grouped map with the definition of all
// explicitly created indexes of the specified table
// (the indexes backing the primary key and the inline constraints are excluded).
//
// Note: This method doesn't return an error on nonexisting table.
func (app *BaseApp) TableIndexes(tableName string) (map[string]string, error) {
	indexes := []struct {
		Name string
		Sql  string
	}{}

	err := app.ConcurrentDB().NewQuery(`
		SELECT c.[[relname]] AS [[name]], pg_get_indexdef(i.[[indexrelid]]) AS [[sql]]
		FROM {{pg_index}} i
		JOIN {{pg_class}} c ON c.[[oid]] = i.[[indexrelid]]
		WHERE i.[[indrelid]] = to_regclass(quote_ident({:tableName}))
			AND NOT EXISTS (SELECT 1 FROM {{pg_constraint}} WHERE [[conindid]] = i.[[indexrelid]])
	`).
		Bind(dbx.Params{"tableName": tableName}).
		All(&indexes)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string, len(indexes))

	for _, idx := range indexes {
		result[idx.Name] = idx.Sql
	}

	return result, nil
}

// DeleteTable drops the specified table.
//
// This method is a no-op if a table with the provided name doesn't exist.
//
// NB! Be aware that this method is vulnerable to SQL injection and the
// "dangerousTableName" argument must come only from trusted input!
func (app *BaseApp) DeleteTable(dangerousTableName string) error {
	// the views that depend on the table are dropped too (and recreated if possible)
	// because unlike SQLite Postgres doesn't allow leaving them broken
	return withDependentViews(app, dangerousTableName, func(txApp App) error {
		_, err := txApp.NonconcurrentDB().NewQuery(fmt.Sprintf(
			"DROP TABLE IF EXISTS {{%s}}",
			dangerousTableName,
		)).Execute()

		return err
	})
}

// HasTable checks if a table (or view) with the provided name exists (case insensitive).
// in the data.db.
func (app *BaseApp) HasTable(tableName string) bool {
	return app.hasTable(app.ConcurrentDB(), tableName)
}

// AuxHasTable checks if a table (or view) with the provided name exists (case insensitive)
// in the auixiliary.db.
func (app *BaseApp) AuxHasTable(tableName string) bool {
	return app.hasTable(app.AuxConcurrentDB(), tableName)
}

func (app *BaseApp) hasTable(db dbx.Builder, tableName string) bool {
	var exists int

	// tables, partitioned tables, views and materialized views visible in the current search_path
	err := db.NewQuery(`
		SELECT (1)
		FROM {{pg_class}} c
		JOIN {{pg_namespace}} n ON n.[[oid]] = c.[[relnamespace]]
		WHERE c.[[relkind]] IN ('r', 'p', 'v', 'm')
			AND n.[[nspname]] = ANY(current_schemas(false))
			AND LOWER(c.[[relname]]) = LOWER({:tableName})
		LIMIT 1
	`).
		Bind(dbx.Params{"tableName": tableName}).
		Row(&exists)

	return err == nil && exists > 0
}

// Vacuum executes VACUUM on the main database.
//
// Postgres reclaims the space automatically with autovacuum so calling it is rarely needed.
func (app *BaseApp) Vacuum() error {
	return app.vacuum(app.NonconcurrentDB())
}

// AuxVacuum executes VACUUM on the auxiliary (logs) database.
func (app *BaseApp) AuxVacuum() error {
	return app.vacuum(app.AuxNonconcurrentDB())
}

func (app *BaseApp) vacuum(db dbx.Builder) error {
	_, err := db.NewQuery("VACUUM").Execute()

	return err
}
