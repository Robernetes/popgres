//go:build !no_default_driver

package core

import (
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pocketbase/dbx"
)

// DefaultDBConnect opens a PostgreSQL connection pool for the provided
// connection string (URL or keyword/value DSN) using the pgx stdlib driver.
//
// popgres: the pgx Postgres driver instead of modernc.org/sqlite
func DefaultDBConnect(dsn string) (*dbx.DB, error) {
	return dbx.Open("pgx", dsn)
}
