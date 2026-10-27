package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
)

func requirePgTools(t *testing.T) {
	t.Helper()

	for _, tool := range []struct{ name, env string }{
		{"pg_dump", core.PgDumpEnv},
		{"pg_restore", core.PgRestoreEnv},
	} {
		if os.Getenv(tool.env) != "" {
			continue
		}
		if _, err := exec.LookPath(tool.name); err != nil {
			t.Skip("popgres: " + tool.name + " is not available")
		}
	}
}

// collectionsSnapshot returns the number of records and the json of the
// records of all non-view collections.
func collectionsSnapshot(t *testing.T, app core.App) (map[string]int, map[string]string) {
	t.Helper()

	collections, err := app.FindAllCollections(core.CollectionTypeBase, core.CollectionTypeAuth)
	if err != nil {
		t.Fatal(err)
	}

	totals := map[string]int{}
	records := map[string]string{}

	for _, c := range collections {
		list, err := app.FindAllRecords(c)
		if err != nil {
			t.Fatal(err)
		}

		totals[c.Name] = len(list)

		exported := make(map[string]any, len(list))
		for _, r := range list {
			// include the hidden fields too (eg. passwords and tokenKeys)
			exported[r.Id] = r.FieldsData()
		}

		raw, err := json.Marshal(exported)
		if err != nil {
			t.Fatal(err)
		}
		records[c.Name] = string(raw)
	}

	return totals, records
}

type rlsPrivilegesState struct {
	Grants      int  `db:"grants"`
	Policies    int  `db:"policies"`
	SchemaUsage bool `db:"schema_usage"`
	// whether a helper function (revoked from PUBLIC) is executable by the API role
	HelperExecutable bool `db:"helper_executable"`
	State            bool `db:"state"`
}

// rlsPrivileges returns the RLS privileges and policies of the app database.
func rlsPrivileges(t *testing.T, app core.App) rlsPrivilegesState {
	t.Helper()

	state := rlsPrivilegesState{}

	err := app.DB().NewQuery(`
		WITH roles AS (
			SELECT oid FROM pg_roles WHERE rolname IN ('popgres_anon', 'popgres_authenticated', 'popgres_api')
		)
		SELECT
			(SELECT count(*) FROM pg_class c, aclexplode(c.relacl) a
				WHERE c.relnamespace = current_schema()::regnamespace AND a.grantee IN (SELECT oid FROM roles))
			+ (SELECT count(*) FROM pg_attribute at JOIN pg_class c ON c.oid = at.attrelid, aclexplode(at.attacl) a
				WHERE c.relnamespace = current_schema()::regnamespace AND a.grantee IN (SELECT oid FROM roles)) AS grants,
			(SELECT count(*) FROM pg_policies WHERE schemaname = current_schema()) AS policies,
			has_schema_privilege('popgres_anon', 'popgres_rls', 'USAGE') AS schema_usage,
			has_function_privilege('popgres_api', 'popgres_rls.base64url_decode(text)', 'EXECUTE') AS helper_executable,
			EXISTS (SELECT 1 FROM _params WHERE id = 'popgresRLS') AS state
	`).One(&state)
	if err != nil {
		t.Fatal(err)
	}

	return state
}

func TestBackupAndRestore(t *testing.T) {
	requirePgTools(t)

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	expectedTotals, expectedRecords := collectionsSnapshot(t, app)

	expectedRLS := rlsPrivileges(t, app)
	if expectedRLS.Grants == 0 || expectedRLS.Policies == 0 || !expectedRLS.SchemaUsage || expectedRLS.HelperExecutable || !expectedRLS.State {
		t.Fatalf("Unexpected RLS state of the test app: %+v", expectedRLS)
	}

	if err := app.CreateBackup(context.Background(), "e2e_backup.zip"); err != nil {
		t.Fatalf("Failed to create the backup: %v", err)
	}

	zipPath := filepath.Join(app.DataDir(), core.LocalBackupsDirName, "e2e_backup.zip")
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatalf("Missing backup archive: %v", err)
	}

	// restore in a new empty database and data directory
	db, err := tests.NewTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer tests.DropTestDatabase(db.Name)

	dataDir := t.TempDir()

	if err := core.RestoreBackupArchive(context.Background(), zipPath, db.URL, "", dataDir); err != nil {
		t.Fatalf("Failed to restore the backup: %v", err)
	}

	// the dump has no privileges (they depend on the server roles),
	// so the external roles have no access until the app reinstalls them
	restoredDB, err := dbx.Open("pgx", db.URL)
	if err != nil {
		t.Fatal(err)
	}
	var restoredGrants int
	err = restoredDB.NewQuery(`
		SELECT count(*) FROM pg_attribute at JOIN pg_class c ON c.oid = at.attrelid, aclexplode(at.attacl) a
		WHERE c.relnamespace = current_schema()::regnamespace
		AND a.grantee IN (SELECT oid FROM pg_roles WHERE rolname IN ('popgres_anon', 'popgres_authenticated', 'popgres_api'))
	`).Row(&restoredGrants)
	restoredDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	if restoredGrants != 0 {
		t.Fatalf("Expected no RLS grants before the app start, got %d", restoredGrants)
	}

	restored := core.NewBaseApp(core.BaseAppConfig{
		DataDir:          dataDir,
		DatabaseURL:      db.URL,
		EncryptionEnv:    "pb_test_env",
		DataMaxOpenConns: 2,
		AuxMaxOpenConns:  1,
	})
	if err := restored.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer restored.ClearBootstrap()

	totals, records := collectionsSnapshot(t, restored)

	// the RLS privileges are reinstalled on the app start
	if rls := rlsPrivileges(t, restored); rls != expectedRLS {
		t.Fatalf("Expected the restored RLS state %+v, got %+v", expectedRLS, rls)
	}

	if len(totals) != len(expectedTotals) {
		t.Fatalf("Expected %d collections, got %d", len(expectedTotals), len(totals))
	}

	for name, total := range expectedTotals {
		if totals[name] != total {
			t.Errorf("Expected %d %s records, got %d", total, name, totals[name])
		}
		if records[name] != expectedRecords[name] {
			t.Errorf("The restored %s records differ\nexpected: %s\ngot:      %s", name, expectedRecords[name], records[name])
		}
	}

	// the logs (_aux schema) are part of the main dump
	var totalLogs int
	if err := restored.AuxDB().Select("count(*)").From("_logs").Row(&totalLogs); err != nil {
		t.Fatalf("Failed to count the restored logs: %v", err)
	}

	// the storage files
	storageFile := filepath.Join(dataDir, core.LocalStorageDirName, "wsmn24bux7wo113", "84nmscqy84lsi1t", "test_d61b33QdDU.txt")
	if _, err := os.Stat(storageFile); err != nil {
		t.Fatalf("Missing restored storage file: %v", err)
	}

	// the backups dir is not part of the backup
	if _, err := os.Stat(filepath.Join(dataDir, core.LocalBackupsDirName)); err == nil {
		t.Fatal("Didn't expect the backups dir to be restored")
	}

	// the app restore is not supported (only the CLI command)
	if err := app.RestoreBackup(context.Background(), "e2e_backup.zip"); err != core.ErrRestoreNotSupported {
		t.Fatalf("Expected ErrRestoreNotSupported, got %v", err)
	}
}
