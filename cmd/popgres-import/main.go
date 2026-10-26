// Command popgres-import imports a PocketBase pb_data directory
// (data.db, auxiliary.db and the local storage files) into a popgres database.
//
// It is a separate binary so that the main popgres binary doesn't link any SQLite driver.
//
// Usage:
//
//	DATABASE_URL=postgres://... popgres-import [flags] <pb_data dir>
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Robernetes/popgres/cmd/popgres-import/internal/importer"
	"github.com/Robernetes/popgres/core"

	_ "github.com/Robernetes/popgres/migrations"
)

func main() {
	var (
		databaseURL   string
		dataDir       string
		encryptionEnv string
		skipLogs      bool
		force         bool
		debug         bool
	)

	flag.StringVar(&databaseURL, "db", os.Getenv(core.DatabaseURLEnv), "the target popgres database connection string (default DATABASE_URL)")
	flag.StringVar(&dataDir, "dir", "", "the target popgres_data directory where the storage files will be copied (the files are not copied if empty)")
	flag.StringVar(&encryptionEnv, "encryptionEnv", "", "the env variable with the encryption key of the PocketBase settings (if they were encrypted)")
	flag.BoolVar(&skipLogs, "skipLogs", false, "skip the import of the request logs")
	flag.BoolVar(&force, "force", false, "import into a database that already has collections (the imported collections and records are replaced, so the import can be repeated)")
	flag.BoolVar(&debug, "debug", false, "print the executed SQL statements")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: popgres-import [flags] <pb_data dir>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	pbDataDir := flag.Arg(0)

	if databaseURL == "" {
		log.Fatal("missing target database: set the DATABASE_URL env variable or the -db flag")
	}

	tempDir, err := os.MkdirTemp("", "popgres_import_")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	appDataDir := dataDir
	if appDataDir == "" {
		appDataDir = tempDir
	}

	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:       appDataDir,
		DatabaseURL:   databaseURL,
		EncryptionEnv: encryptionEnv,
		IsDev:         debug,
	})
	if err := app.Bootstrap(); err != nil {
		log.Fatal(err)
	}
	defer app.ClearBootstrap()

	report, err := importer.Import(app, pbDataDir, importer.Options{
		SkipLogs: skipLogs,
		Force:    force,
	})
	if err != nil {
		log.Fatal(err)
	}

	if dataDir != "" {
		if err := importer.CopyStorage(pbDataDir, dataDir); err != nil {
			log.Fatal(err)
		}
	}

	if dataDir != "" && report.Storage != "" && !app.Settings().S3.Enabled {
		report.Storage = "local storage, copied to " + dataDir
	}

	fmt.Print(report.String())

	if report.HasManualChanges() {
		os.Exit(3)
	}
}
