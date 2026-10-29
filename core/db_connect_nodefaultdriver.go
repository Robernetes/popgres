//go:build no_default_driver

package core

import "github.com/pocketbase/dbx"

// popgres: the argument is a Postgres connection string instead of a SQLite db path
func DefaultDBConnect(dsn string) (*dbx.DB, error) {
	panic("DBConnect config option must be set when the no_default_driver tag is used!")
}
