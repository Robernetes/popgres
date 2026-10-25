package search

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Robernetes/popgres/tools/dbutils"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pocketbase/dbx"
)

// note: the tests package (with the regular test databases helpers) cannot be
// imported here because it depends on this package, so this is a minimal copy.

// OpenTestPgDB creates a new empty test database from the POPGRES_TEST_URL
// template and returns its connection together with a cleanup function
// that closes the connection and drops the database.
//
// It is exported only for the external search_test package.
func OpenTestPgDB() (*dbx.DB, func(), error) {
	templateURL := os.Getenv("POPGRES_TEST_URL")
	if templateURL == "" {
		return nil, nil, errors.New("missing POPGRES_TEST_URL env variable")
	}

	u, err := url.Parse(templateURL)
	if err != nil {
		return nil, nil, err
	}
	templateName := strings.TrimPrefix(u.Path, "/")

	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return nil, nil, err
	}
	name := "popgres_t_" + hex.EncodeToString(random)

	adminURL := *u
	adminURL.Path = "/postgres"
	admin, err := sql.Open("pgx", adminURL.String())
	if err != nil {
		return nil, nil, err
	}

	_, err = admin.Exec(fmt.Sprintf(`CREATE DATABASE %q TEMPLATE %q`, name, templateName))
	if err != nil {
		admin.Close()
		return nil, nil, err
	}

	dropDB := func() {
		_, _ = admin.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
		admin.Close()
	}

	dbURL := *u
	dbURL.Path = "/" + name
	db, err := dbx.Open("pgx", dbURL.String())
	if err != nil {
		dropDB()
		return nil, nil, err
	}
	db.DB().SetMaxOpenConns(2)

	// the popgres helper SQL functions (json, strftime, etc.)
	if _, err := db.NewQuery(dbutils.PostgresFunctionsSQL).Execute(); err != nil {
		db.Close()
		dropDB()
		return nil, nil, err
	}

	return db, func() {
		db.Close()
		dropDB()
	}, nil
}
