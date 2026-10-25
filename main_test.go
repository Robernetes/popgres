package popgres

import (
	"fmt"
	"os"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
)

func TestMain(m *testing.M) {
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
