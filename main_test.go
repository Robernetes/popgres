package popgres

import (
	"fmt"
	"os"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
)

func TestMain(m *testing.M) {
	// the embedded Postgres tests could run without a Postgres server
	// (eg. the CI job on the operating systems without Postgres)
	if os.Getenv(tests.TestDatabaseURLEnv) == "" && os.Getenv("POPGRES_TEST_EMBEDDED") != "" {
		os.Exit(m.Run())
	}

	// the apps of this package are created from the DATABASE_URL env variable
	db, err := tests.NewTestDatabase()
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to create a test database:", err)
		os.Exit(1)
	}

	os.Setenv(core.DatabaseURLEnv, db.URL)

	code := m.Run()

	_ = tests.DropTestDatabase(db.Name)

	os.Exit(code)
}
