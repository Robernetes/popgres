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

func TestBackupAndRestore(t *testing.T) {
	requirePgTools(t)

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	expectedTotals, expectedRecords := collectionsSnapshot(t, app)

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
