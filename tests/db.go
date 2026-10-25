package tests

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Robernetes/popgres/tools/security"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestDatabaseURLEnv is the env variable with the connection string of the
// template database used by the tests (eg. postgres://user:pass@localhost:5433/popgres_test).
//
// The template database itself is never modified. Each test app works on its own
// "popgres_t_*" copy that is created and dropped through the "postgres"
// maintenance database of the same server.
const TestDatabaseURLEnv = "POPGRES_TEST_URL"

const (
	// TestDatabasePrefix is the name prefix of the per test databases.
	TestDatabasePrefix = "popgres_t_"

	// FixturesDatabasePrefix is the name prefix of the databases with preloaded fixtures.
	FixturesDatabasePrefix = "popgres_fx_"

	maintenanceDatabase = "postgres"
)

var (
	adminDBOnce sync.Once
	adminDB     *sql.DB
	adminDBErr  error
)

// TestDatabase describes a single temporary test database.
type TestDatabase struct {
	// Name is the database name.
	Name string

	// URL is the connection string of the database.
	URL string
}

// NewTestDatabase creates a new empty database (only the extensions and no
// tables) from the TestDatabaseURLEnv template database.
//
// It is the caller's responsibility to call [DropTestDatabase] when the database is no longer needed.
func NewTestDatabase() (*TestDatabase, error) {
	templateURL := os.Getenv(TestDatabaseURLEnv)
	if templateURL == "" {
		return nil, fmt.Errorf("missing %s env variable (eg. postgres://user:pass@localhost:5433/popgres_test)", TestDatabaseURLEnv)
	}

	templateName, err := databaseNameFromURL(templateURL)
	if err != nil {
		return nil, err
	}

	return newTestDatabaseFrom(templateName)
}

// newTestDatabaseFrom creates a new test database as a copy of the specified template database.
func newTestDatabaseFrom(templateName string) (*TestDatabase, error) {
	templateURL := os.Getenv(TestDatabaseURLEnv)
	if templateURL == "" {
		return nil, fmt.Errorf("missing %s env variable (eg. postgres://user:pass@localhost:5433/popgres_test)", TestDatabaseURLEnv)
	}

	db, err := testAdminDB(templateURL)
	if err != nil {
		return nil, err
	}

	name := TestDatabasePrefix + security.RandomStringWithAlphabet(12, "abcdefghijklmnopqrstuvwxyz0123456789")

	createSQL := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", quoteIdent(name), quoteIdent(templateName))

	// the template cannot have other connections during the copy so
	// retry in case another process is still finishing with it
	for attempt := 1; ; attempt++ {
		_, err = db.Exec(createSQL)
		if !isTemplateBusyError(err) || attempt >= 20 {
			break
		}
		time.Sleep(time.Duration(attempt*50) * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create test database %s from %s: %w", name, templateName, err)
	}

	dsn, err := replaceDatabaseName(templateURL, name)
	if err != nil {
		return nil, errors.Join(err, DropTestDatabase(name))
	}

	return &TestDatabase{Name: name, URL: dsn}, nil
}

// DropTestDatabase drops the specified test database (closing any remaining connection to it).
//
// Only the databases with the TestDatabasePrefix or FixturesDatabasePrefix are allowed to be dropped.
func DropTestDatabase(name string) error {
	if !strings.HasPrefix(name, TestDatabasePrefix) && !strings.HasPrefix(name, FixturesDatabasePrefix) {
		return fmt.Errorf("refusing to drop the non-test database %q", name)
	}

	db, err := testAdminDB(os.Getenv(TestDatabaseURLEnv))
	if err != nil {
		return err
	}

	// the logical replication slots of the database (eg. of the realtime
	// replication tests) are global objects that would block the drop
	if err := dropReplicationSlots(db, name); err != nil {
		return err
	}

	_, err = db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(name)))

	return err
}

// dropReplicationSlots drops the replication slots of the specified
// database (terminating their consumers first).
func dropReplicationSlots(db *sql.DB, name string) error {
	var lastErr error

	for range 20 {
		_, err := db.Exec(
			"SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE database = $1 AND active_pid IS NOT NULL",
			name,
		)
		if err != nil {
			return err
		}

		// note: the slot could be still active right after the termination of its consumer
		_, lastErr = db.Exec("SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE database = $1", name)
		if lastErr == nil {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return lastErr
}

// DropOrphanTestDatabases drops all test and fixtures databases of the
// TestDatabaseURLEnv server and returns the names of the dropped ones.
//
// It must be used only when there are no running tests (eg. "make test-clean").
func DropOrphanTestDatabases() ([]string, error) {
	db, err := testAdminDB(os.Getenv(TestDatabaseURLEnv))
	if err != nil {
		return nil, err
	}

	names := []string{}

	// note: starts_with instead of LIKE because "_" is a LIKE wildcard
	rows, err := db.Query(
		"SELECT datname FROM pg_database WHERE starts_with(datname, $1) OR starts_with(datname, $2) ORDER BY datname",
		TestDatabasePrefix,
		FixturesDatabasePrefix,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	dropped := make([]string, 0, len(names))

	for _, name := range names {
		if err := DropTestDatabase(name); err != nil {
			return dropped, err
		}
		dropped = append(dropped, name)
	}

	return dropped, nil
}

// testAdminDB returns a shared small connection pool to the maintenance
// database of the server specified by the provided connection string.
func testAdminDB(templateURL string) (*sql.DB, error) {
	adminDBOnce.Do(func() {
		if templateURL == "" {
			adminDBErr = fmt.Errorf("missing %s env variable", TestDatabaseURLEnv)
			return
		}

		var dsn string
		dsn, adminDBErr = replaceDatabaseName(templateURL, maintenanceDatabase)
		if adminDBErr != nil {
			return
		}

		adminDB, adminDBErr = sql.Open("pgx", dsn)
		if adminDBErr != nil {
			return
		}

		adminDB.SetMaxOpenConns(2)
		adminDB.SetMaxIdleConns(1)
	})

	return adminDB, adminDBErr
}

func databaseNameFromURL(dsn string) (string, error) {
	if isURLDSN(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}

		name := strings.TrimPrefix(u.Path, "/")
		if name == "" {
			return "", fmt.Errorf("missing database name in %s", TestDatabaseURLEnv)
		}

		return name, nil
	}

	for _, part := range strings.Fields(dsn) {
		if v, ok := strings.CutPrefix(part, "dbname="); ok {
			return strings.Trim(v, "'"), nil
		}
	}

	return "", fmt.Errorf("missing dbname in %s", TestDatabaseURLEnv)
}

func replaceDatabaseName(dsn string, name string) (string, error) {
	if isURLDSN(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}

		u.Path = "/" + name

		return u.String(), nil
	}

	// keyword/value form (the last dbname occurrence wins)
	return strings.TrimSpace(dsn) + " dbname='" + name + "'", nil
}

func isURLDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// DropOrphanTestRoles drops the leftover test roles ("popgres_t_*", eg. created
// by the replication fallback tests) of the TestDatabaseURLEnv server and
// returns the names of the dropped ones.
//
// It must be used only when there are no running tests and after the
// orphan test databases drop (eg. "make test-clean").
func DropOrphanTestRoles() ([]string, error) {
	db, err := testAdminDB(os.Getenv(TestDatabaseURLEnv))
	if err != nil {
		return nil, err
	}

	names := []string{}

	rows, err := db.Query("SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1)", TestDatabasePrefix)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	rows.Close()

	dropped := make([]string, 0, len(names))
	for _, name := range names {
		if _, err := db.Exec("DROP ROLE IF EXISTS " + quoteIdent(name)); err != nil {
			return dropped, err
		}
		dropped = append(dropped, name)
	}

	return dropped, nil
}
