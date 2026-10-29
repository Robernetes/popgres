package core

import (
	"github.com/pocketbase/dbx"
)

// dependentView is a view that depends (directly or indirectly) on another relation.
type dependentView struct {
	Name  string `db:"name"`
	SQL   string `db:"sql"`
	Depth int    `db:"depth"`
}

// findDependentViews returns the views that depend (directly or through other
// views) on the specified table or view, ordered by their dependency depth.
//
// Unlike SQLite, Postgres doesn't allow dropping or altering a relation that is
// used by a view, so the dependent views have to be dropped and recreated.
func findDependentViews(app App, relationName string) ([]*dependentView, error) {
	views := []*dependentView{}

	err := app.DB().NewQuery(`
		WITH RECURSIVE deps AS (
			SELECT v.[[oid]], v.[[relname]], 1 AS [[depth]]
			FROM {{pg_depend}} d
			JOIN {{pg_rewrite}} r ON r.[[oid]] = d.[[objid]]
			JOIN {{pg_class}} v ON v.[[oid]] = r.[[ev_class]]
			WHERE d.[[refobjid]] = to_regclass(quote_ident({:name}))
				AND d.[[classid]] = 'pg_rewrite'::regclass
				AND v.[[relkind]] = 'v'
				AND v.[[oid]] <> d.[[refobjid]]
			UNION
			SELECT v.[[oid]], v.[[relname]], deps.[[depth]] + 1
			FROM deps
			JOIN {{pg_depend}} d ON d.[[refobjid]] = deps.[[oid]]
			JOIN {{pg_rewrite}} r ON r.[[oid]] = d.[[objid]]
			JOIN {{pg_class}} v ON v.[[oid]] = r.[[ev_class]]
			WHERE d.[[classid]] = 'pg_rewrite'::regclass
				AND v.[[relkind]] = 'v'
				AND v.[[oid]] <> d.[[refobjid]]
				AND deps.[[depth]] < 50
		)
		SELECT
			[[relname]] AS [[name]],
			format('CREATE VIEW %I AS %s', [[relname]], pg_get_viewdef([[oid]])) AS [[sql]],
			max([[depth]]) AS [[depth]]
		FROM deps
		GROUP BY [[oid]], [[relname]]
		ORDER BY max([[depth]]) ASC, [[relname]] ASC
	`).Bind(dbx.Params{"name": relationName}).All(&views)

	return views, err
}

// findSchemaViews returns all views of the current schema ordered by their
// dependency depth (a view is listed after the views that it uses).
func findSchemaViews(app App) ([]*dependentView, error) {
	views := []*dependentView{}

	err := app.DB().NewQuery(`
		WITH RECURSIVE deps AS (
			SELECT c.[[oid]], 0 AS [[depth]]
			FROM {{pg_class}} c
			JOIN {{pg_namespace}} n ON n.[[oid]] = c.[[relnamespace]]
			WHERE c.[[relkind]] = 'v' AND n.[[nspname]] = current_schema()
			UNION
			SELECT v.[[oid]], deps.[[depth]] + 1
			FROM deps
			JOIN {{pg_depend}} d ON d.[[refobjid]] = deps.[[oid]]
			JOIN {{pg_rewrite}} r ON r.[[oid]] = d.[[objid]]
			JOIN {{pg_class}} v ON v.[[oid]] = r.[[ev_class]]
			WHERE d.[[classid]] = 'pg_rewrite'::regclass
				AND v.[[relkind]] = 'v'
				AND v.[[oid]] <> d.[[refobjid]]
				AND deps.[[depth]] < 50
		)
		SELECT
			c.[[relname]] AS [[name]],
			format('CREATE VIEW %I AS %s', c.[[relname]], pg_get_viewdef(c.[[oid]])) AS [[sql]],
			max(deps.[[depth]]) AS [[depth]]
		FROM deps
		JOIN {{pg_class}} c ON c.[[oid]] = deps.[[oid]]
		GROUP BY c.[[oid]], c.[[relname]]
		ORDER BY max(deps.[[depth]]) ASC, c.[[relname]] ASC
	`).All(&views)

	return views, err
}

// withDependentViews executes fn (that is expected to drop or alter the
// specified relation) in a transaction after dropping the relation dependent
// views and recreates them afterwards.
//
// A dependent view that cannot be recreated (eg. because it uses a dropped
// column) is skipped, the same as the broken views that SQLite would keep.
func withDependentViews(app App, relationName string, fn func(txApp App) error) error {
	return app.RunInTransaction(func(txApp App) error {
		views, err := findDependentViews(txApp, relationName)
		if err != nil {
			return err
		}

		// drop from the most dependent one
		for i := len(views) - 1; i >= 0; i-- {
			_, err := txApp.DB().NewQuery("DROP VIEW IF EXISTS {{" + views[i].Name + "}}").Execute()
			if err != nil {
				return err
			}
		}

		if err := fn(txApp); err != nil {
			return err
		}

		for _, view := range views {
			// each view is recreated in its own savepoint so that a failed
			// recreation doesn't abort the whole transaction
			// (note: the nested RunInTransaction calls reuse the same transaction)
			if _, err := txApp.DB().NewQuery("SAVEPOINT popgres_dependent_view").Execute(); err != nil {
				return err
			}

			if _, err := txApp.DB().NewQuery(view.SQL).Execute(); err != nil {
				if _, err := txApp.DB().NewQuery("ROLLBACK TO SAVEPOINT popgres_dependent_view").Execute(); err != nil {
					return err
				}
			}

			if _, err := txApp.DB().NewQuery("RELEASE SAVEPOINT popgres_dependent_view").Execute(); err != nil {
				return err
			}
		}

		return nil
	})
}
