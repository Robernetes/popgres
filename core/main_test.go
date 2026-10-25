package core_test

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
)

func TestMain(m *testing.M) {
	var mu sync.Mutex
	created := []string{}

	// the apps created with core.NewBaseApp without a connection string
	// (eg. to test the bootstrap) get their own empty test database
	core.SetTestDatabaseURLFunc(func() string {
		db, err := tests.NewTestDatabase()
		if err != nil {
			panic(fmt.Errorf("failed to create a test database: %w", err))
		}

		mu.Lock()
		created = append(created, db.Name)
		mu.Unlock()

		return db.URL
	})

	code := m.Run()

	for _, name := range created {
		_ = tests.DropTestDatabase(name)
	}

	os.Exit(code)
}
