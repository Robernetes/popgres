package tests

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Robernetes/popgres/core"
)

// FixturesFile is the name of the SQL fixtures file in the tests data directory
// (generated with scripts/gen-fixtures.sh).
const FixturesFile = "data.pg.sql"

var (
	fixturesOnce     sync.Once
	fixturesTemplate string
	fixturesErr      error
)

// testsDataDir returns the absolute path of the default tests data directory.
func testsDataDir() string {
	_, currentFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(currentFile), "data")
}

// schemaFingerprint returns the identifiers of the schema created on top of
// the fixtures (the system migrations and their source files, if available)
// so that the fixtures template is recreated when they change.
func schemaFingerprint() []byte {
	var b strings.Builder

	for _, m := range core.SystemMigrations.Items() {
		b.WriteString(m.File)
		b.WriteString("\n")
	}

	_, currentFile, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(currentFile))

	sources, _ := filepath.Glob(filepath.Join(root, "migrations", "*.go"))
	rlsSources, _ := filepath.Glob(filepath.Join(root, "core", "rls*.go"))
	for _, path := range append(sources, rlsSources...) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if content, err := os.ReadFile(path); err == nil {
			b.Write(content)
		}
	}

	return []byte(b.String())
}

// NewFixturesTestDatabase creates a new test database with the default
// test fixtures (see [FixturesFile]).
//
// It is the caller's responsibility to call [DropTestDatabase] when the database is no longer needed.
func NewFixturesTestDatabase() (*TestDatabase, error) {
	template, err := fixturesTemplateName()
	if err != nil {
		return nil, err
	}

	return newTestDatabaseFrom(template)
}

// fixturesTemplateName returns the name of the database template with the
// loaded fixtures, creating it if missing.
//
// The template is created only once per fixtures content and shared by all
// test processes (go test runs each package in a separate process).
func fixturesTemplateName() (string, error) {
	fixturesOnce.Do(func() {
		fixturesTemplate, fixturesErr = ensureFixturesTemplate()
	})

	return fixturesTemplate, fixturesErr
}

func ensureFixturesTemplate() (string, error) {
	fixtures, err := os.ReadFile(filepath.Join(testsDataDir(), FixturesFile))
	if err != nil {
		return "", fmt.Errorf("failed to read the test fixtures: %w", err)
	}

	sum := sha256.Sum256(append(fixtures, schemaFingerprint()...))
	name := FixturesDatabasePrefix + hex.EncodeToString(sum[:])[:16]

	admin, err := testAdminDB(os.Getenv(TestDatabaseURLEnv))
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// session level advisory locks require a dedicated connection
	conn, err := admin.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	lockKey := int64(binary.BigEndian.Uint64(sum[:8]))
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return "", err
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)

	var exists bool
	err = conn.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := createFixturesTemplate(name, string(fixtures)); err != nil {
			return "", errors.Join(err, DropTestDatabase(name))
		}
	}

	// mark the template as recently used so that it is not dropped
	// as stale by a concurrent run with different fixtures
	_, _ = conn.ExecContext(ctx, fmt.Sprintf(
		"COMMENT ON DATABASE %s IS '%s%d'",
		quoteIdent(name),
		fixturesUsedCommentPrefix,
		time.Now().Unix(),
	))

	dropStaleFixturesTemplates(conn, name)

	return name, nil
}

const (
	fixturesUsedCommentPrefix = "popgres fixtures template, last used at "

	// fixturesStaleAfter is the minimum time after the last use
	// of a fixtures template of another version before it is dropped
	fixturesStaleAfter = time.Hour
)

func createFixturesTemplate(name string, fixtures string) error {
	db, err := newTestDatabaseFrom(rootTemplateName())
	if err != nil {
		return err
	}

	admin, err := testAdminDB(os.Getenv(TestDatabaseURLEnv))
	if err != nil {
		return err
	}

	// rename to the fixtures template name
	if _, err := admin.Exec(fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", quoteIdent(db.Name), quoteIdent(name))); err != nil {
		return errors.Join(err, DropTestDatabase(db.Name))
	}

	url, err := replaceDatabaseName(db.URL, name)
	if err != nil {
		return err
	}

	// load the fixtures on a dedicated connection because the dump changes
	// some session settings (eg. search_path)
	if err := execSQLFile(url, fixtures); err != nil {
		return fmt.Errorf("failed to load the test fixtures: %w", err)
	}

	// apply the system migrations that are not part of the fixtures yet
	tempDir, err := os.MkdirTemp("", "popgres_fixtures_")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:          tempDir,
		DatabaseURL:      url,
		DataMaxOpenConns: 2,
		AuxMaxOpenConns:  1,
	})
	if err := app.Bootstrap(); err != nil {
		return fmt.Errorf("failed to bootstrap the fixtures database: %w", err)
	}

	return app.ClearBootstrap()
}

func execSQLFile(url string, content string) error {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()

	db.SetMaxOpenConns(1)

	_, err = db.Exec(content)

	return err
}

// dropStaleFixturesTemplates drops (best effort) the fixtures templates
// of previous fixtures versions.
func dropStaleFixturesTemplates(conn *sql.Conn, current string) {
	rows, err := conn.QueryContext(
		context.Background(),
		"SELECT datname, COALESCE(shobj_description(oid, 'pg_database'), '') FROM pg_database WHERE starts_with(datname, $1) AND datname <> $2",
		FixturesDatabasePrefix,
		current,
	)
	if err != nil {
		return
	}

	names := []string{}
	for rows.Next() {
		var name, comment string
		if rows.Scan(&name, &comment) != nil {
			continue
		}

		// keep the templates that could be in use by another run
		lastUsed, _ := strconv.ParseInt(strings.TrimPrefix(comment, fixturesUsedCommentPrefix), 10, 64)
		if time.Since(time.Unix(lastUsed, 0)) < fixturesStaleAfter {
			continue
		}

		names = append(names, name)
	}
	rows.Close()

	for _, name := range names {
		// no FORCE so that a template that is being cloned by another run is not interrupted
		_, _ = conn.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+quoteIdent(name))
	}
}

func rootTemplateName() string {
	name, _ := databaseNameFromURL(os.Getenv(TestDatabaseURLEnv))
	return name
}

// isTemplateBusyError reports whether err is the error returned
// by CREATE DATABASE when the template database has other connections.
func isTemplateBusyError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is being accessed by other users")
}
