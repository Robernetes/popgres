package core

import (
	"errors"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tools/security"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pocketbase/dbx"
)

// clusterSecretParamKey is the _params key of the secret that authenticates
// the cluster messages of the nodes (see [cluster.Config.Secret]).
//
// The _params table is not accessible by the RLS roles.
const clusterSecretParamKey = "popgresCluster"

// loadClusterSecret returns the cluster secret, creating it on the first call.
//
// It returns [cluster.ErrNotReady] if the _params table doesn't exist yet
// (eg. a new database before the system migrations).
func (app *BaseApp) loadClusterSecret() (string, error) {
	db, ok := app.dataDB.(*dbx.DB)
	if !ok || db == nil {
		return "", errors.New("the app database is not initialized")
	}

	var secret string

	err := db.Transactional(func(tx *dbx.Tx) error {
		// don't wait for long if the table is locked (eg. by a running migration
		// that publishes a cluster message and waits for this call)
		if _, err := tx.NewQuery("SET LOCAL lock_timeout = '2s'").Execute(); err != nil {
			return err
		}

		_, err := tx.NewQuery(`
			INSERT INTO {{_params}} ([[id]], [[value]]) VALUES ({:id}, {:value})
			ON CONFLICT ([[id]]) DO NOTHING
		`).Bind(dbx.Params{
			"id":    clusterSecretParamKey,
			"value": security.RandomString(50),
		}).Execute()
		if err != nil {
			return err
		}

		return tx.Select("value").From(paramsTable).Where(dbx.HashExp{"id": clusterSecretParamKey}).Row(&secret)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
			return "", cluster.ErrNotReady
		}
		return "", err
	}

	return secret, nil
}
