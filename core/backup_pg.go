package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Robernetes/popgres/tools/archive"
	"github.com/pocketbase/dbx"
)

const (
	// backupDataDumpFilename is the name of the main database dump in a backup archive.
	backupDataDumpFilename = "data.dump"

	// backupAuxDumpFilename is the name of the separate logs database
	// dump in a backup archive (if POPGRES_LOGS_URL is used).
	backupAuxDumpFilename = "auxiliary.dump"

	// PgDumpEnv and PgRestoreEnv are optional env variables with the
	// paths of the pg_dump and pg_restore binaries (default to the ones in PATH).
	PgDumpEnv    = "POPGRES_PG_DUMP"
	PgRestoreEnv = "POPGRES_PG_RESTORE"
)

// ErrPgToolsMissing is returned when the pg_dump or pg_restore binary couldn't be found.
var ErrPgToolsMissing = errors.New("the backups require the pg_dump and pg_restore binaries " +
	"(same or newer major version than the server) in PATH or in the " + PgDumpEnv + " and " + PgRestoreEnv + " env variables")

// ErrRestoreNotSupported is returned by RestoreBackup because restoring a
// Postgres database of a running app (or cluster) is not safe.
var ErrRestoreNotSupported = errors.New("restoring a backup from a running app is not supported, " +
	"stop the app and use the \"popgres restore <backup>\" command instead")

// DatabaseURL returns the connection string of the main database.
func (app *BaseApp) DatabaseURL() string {
	return app.config.DatabaseURL
}

// LogsDatabaseURL returns the connection string of the logs database
// (empty if the logs are stored in the main database).
func (app *BaseApp) LogsDatabaseURL() string {
	return app.config.LogsDatabaseURL
}

func pgToolPath(name string, env string) (string, error) {
	if p := os.Getenv(env); p != "" {
		return p, nil
	}

	p, err := exec.LookPath(name)
	if err != nil {
		return "", ErrPgToolsMissing
	}

	return p, nil
}

var dsnPasswordRegex = regexp.MustCompile(`(?:^|\s)password\s*=\s*('(?:[^'\\]|\\.)*'|\S*)`)

// pgToolConnection returns the connection string to pass as command argument
// (without the password) and the password that should be passed in the
// PGPASSWORD env variable (so that it is not visible in the processes list).
func pgToolConnection(dsn string) (string, string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", "", err
		}

		var password string
		if u.User != nil {
			password, _ = u.User.Password()
			u.User = url.User(u.User.Username())
		}

		return u.String(), password, nil
	}

	var password string
	if m := dsnPasswordRegex.FindStringSubmatch(dsn); len(m) == 2 {
		password = strings.Trim(m[1], "'")
		password = strings.ReplaceAll(strings.ReplaceAll(password, `\'`, `'`), `\\`, `\`)
	}

	return strings.TrimSpace(dsnPasswordRegex.ReplaceAllString(dsn, " ")), password, nil
}

func runPgTool(ctx context.Context, name string, env string, dsn string, args ...string) error {
	bin, err := pgToolPath(name, env)
	if err != nil {
		return err
	}

	conn, password, err := pgToolConnection(dsn)
	if err != nil {
		return err
	}

	if dsn != "" {
		args = append(args, "--dbname", conn)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	if password != "" {
		cmd.Env = append(cmd.Env, "PGPASSWORD="+password)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}

	return nil
}

// dumpDatabase writes a custom format pg_dump of the specified database to outPath.
//
// The privileges are not included because they depend on the roles of the
// server (the RLS privileges are reinstalled after the restore, see [BaseApp.repairRLS]).
//
// The data of the UNLOGGED tables is not included because it is temporary
// (eg. the forwarded requests of the cluster, with their auth headers).
func dumpDatabase(ctx context.Context, dsn string, outPath string) error {
	return runPgTool(ctx, "pg_dump", PgDumpEnv, dsn,
		"--format=custom",
		"--no-owner",
		"--no-privileges",
		"--no-unlogged-table-data",
		"--file", outPath,
	)
}

// restoreDatabaseDump restores a custom format pg_dump into the specified
// database (the existing objects of the dump are dropped first).
//
// The extensions of the dump are not dropped and recreated by pg_restore
// because they are usually owned by a superuser (eg. on managed Postgres
// services the app role can't drop them). They are created instead with
// "CREATE EXTENSION IF NOT EXISTS" before the restore.
func restoreDatabaseDump(ctx context.Context, dsn string, dumpPath string) error {
	listPath := dumpPath + ".list"
	defer os.Remove(listPath)

	err := runPgTool(ctx, "pg_restore", PgRestoreEnv, "", "--list", "--file", listPath, dumpPath)
	if err != nil {
		return err
	}

	rawList, err := os.ReadFile(listPath)
	if err != nil {
		return err
	}

	list, extensions := withoutExtensionEntries(string(rawList))

	if err := os.WriteFile(listPath, []byte(list), 0o600); err != nil {
		return err
	}

	if err := ensureExtensions(ctx, dsn, extensions); err != nil {
		return err
	}

	// the RLS policies of the dump reference the external roles
	// (eg. a restore on a new server)
	if hasPolicyEntries(list) {
		if err := ensureRLSRoles(ctx, dsn); err != nil {
			return err
		}
	}

	return runPgTool(ctx, "pg_restore", PgRestoreEnv, dsn,
		"--clean",
		"--if-exists",
		"--no-owner",
		"--no-privileges",
		"--single-transaction",
		"--exit-on-error",
		"--use-list", listPath,
		dumpPath,
	)
}

var (
	// eg. "3; 3079 16386 EXTENSION - vector " (dumpId; tableoid oid type schema name owner)
	tocExtensionRegex = regexp.MustCompile(`^\s*\d+;\s+\d+\s+\d+\s+EXTENSION\s+\S+\s+(\S+)`)

	// eg. "4296; 0 0 COMMENT - EXTENSION vector "
	tocExtensionCommentRegex = regexp.MustCompile(`^\s*\d+;\s+\d+\s+\d+\s+COMMENT\s+\S+\s+EXTENSION\s+`)

	// eg. "5012; 3256 17201 POLICY public posts rls_list app"
	tocPolicyRegex = regexp.MustCompile(`(?m)^\s*\d+;\s+\d+\s+\d+\s+POLICY\s+`)
)

// hasPolicyEntries reports whether the pg_restore list has row level security policies.
func hasPolicyEntries(list string) bool {
	return tocPolicyRegex.MatchString(list)
}

// ensureRLSRoles creates the external RLS roles if they don't exist.
func ensureRLSRoles(ctx context.Context, dsn string) error {
	db, err := dbx.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.NewQuery(rlsRolesSQL).WithContext(ctx).Execute(); err != nil {
		return rlsRolesError(err)
	}

	return nil
}

// resetRLSState removes the RLS state of the restored database, so that the RLS
// privileges (not part of the dump) are reinstalled on the next app start.
func resetRLSState(ctx context.Context, dsn string) error {
	db, err := dbx.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	_, err = db.Delete(paramsTable, dbx.HashExp{"id": rlsStateParamKey}).WithContext(ctx).Execute()

	return err
}

// withoutExtensionEntries returns the pg_restore list without the
// extension entries and the names of the removed extensions.
func withoutExtensionEntries(list string) (string, []string) {
	lines := strings.Split(list, "\n")
	result := make([]string, 0, len(lines))
	extensions := []string{}

	for _, line := range lines {
		if m := tocExtensionRegex.FindStringSubmatch(line); len(m) == 2 {
			extensions = append(extensions, m[1])
			continue
		}
		if tocExtensionCommentRegex.MatchString(line) {
			continue
		}
		result = append(result, line)
	}

	return strings.Join(result, "\n"), extensions
}

// ensureExtensions creates the specified extensions if they don't exist.
func ensureExtensions(ctx context.Context, dsn string, extensions []string) error {
	if len(extensions) == 0 {
		return nil
	}

	db, err := dbx.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	for _, name := range extensions {
		_, err := db.NewQuery("CREATE EXTENSION IF NOT EXISTS " + db.QuoteSimpleTableName(name)).WithContext(ctx).Execute()
		if err != nil {
			return fmt.Errorf("failed to create the %q extension of the backup (it may require a superuser): %w", name, err)
		}
	}

	return nil
}

// RestoreBackupArchive restores a popgres backup zip archive created with
// [App.CreateBackup] into the specified databases and data directory.
//
// It is intended to be used only when the app is not running
// (eg. by the "popgres restore" command).
//
// The database objects of the dumps are dropped and recreated, and the
// local files of the archive (eg. the storage files) replace the ones in dataDir.
func RestoreBackupArchive(ctx context.Context, zipPath string, databaseURL string, logsDatabaseURL string, dataDir string) error {
	if databaseURL == "" {
		return errors.New("missing database connection string")
	}

	if err := os.MkdirAll(dataDir, os.ModePerm); err != nil {
		return err
	}

	// extract inside the data dir so that the final renames are on the same volume
	tempDir, err := os.MkdirTemp(dataDir, ".popgres_restore_")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	if err := archive.Extract(zipPath, tempDir); err != nil {
		return fmt.Errorf("failed to extract the backup: %w", err)
	}

	dataDump := filepath.Join(tempDir, backupDataDumpFilename)
	if _, err := os.Stat(dataDump); err != nil {
		return fmt.Errorf("invalid popgres backup - missing %s", backupDataDumpFilename)
	}

	if err := restoreDatabaseDump(ctx, databaseURL, dataDump); err != nil {
		return err
	}
	if err := resetRLSState(ctx, databaseURL); err != nil {
		return fmt.Errorf("failed to reset the RLS state of the restored database: %w", err)
	}
	if err := os.Remove(dataDump); err != nil {
		return err
	}

	auxDump := filepath.Join(tempDir, backupAuxDumpFilename)
	if _, err := os.Stat(auxDump); err == nil {
		target := logsDatabaseURL
		if target == "" {
			target = databaseURL
		}
		if err := restoreDatabaseDump(ctx, target, auxDump); err != nil {
			return err
		}
		if err := os.Remove(auxDump); err != nil {
			return err
		}
	}

	// replace the local files (only the ones that are part of the backup)
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		target := filepath.Join(dataDir, entry.Name())
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(tempDir, entry.Name()), target); err != nil {
			return err
		}
	}

	return nil
}
