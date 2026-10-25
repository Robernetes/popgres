package core_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/Robernetes/popgres/tools/store"
	"github.com/pocketbase/dbx"
)

func TestNotifyWatcher_SettingsUpdate(t *testing.T) {
	t.Parallel()

	testEvents := store.New[core.App, int](nil)

	tmpDir, err := os.MkdirTemp("", "pb_notify_test*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// popgres: both instances share the same database
	db, err := tests.NewTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer tests.DropTestDatabase(db.Name)

	app1 := core.NewBaseApp(core.BaseAppConfig{
		DataDir:     tmpDir,
		DatabaseURL: db.URL,
	})
	if err := app1.Bootstrap(); err != nil {
		t.Fatal(err)
	}

	app2 := core.NewBaseApp(core.BaseAppConfig{
		DataDir:     tmpDir,
		DatabaseURL: db.URL,
	})
	if err := app2.Bootstrap(); err != nil {
		t.Fatal(err)
	}

	defer app1.ClearBootstrap()
	defer app2.ClearBootstrap()

	select {
	case <-app2.Cluster().Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("app2 cluster LISTEN timeout")
	}

	timeout := time.After(3 * time.Second)
	done := make(chan struct{})

	app1.OnSettingsReload().BindFunc(func(e *core.SettingsReloadEvent) error {
		testEvents.SetFunc(app1, func(old int) int {
			return old + 1
		})
		return e.Next()
	})

	app2.OnSettingsReload().BindFunc(func(e *core.SettingsReloadEvent) error {
		testEvents.SetFunc(app2, func(old int) int {
			return old + 1
		})

		// popgres: signal after the reloaded settings were applied
		// to avoid racing with the assertions below
		defer func() {
			done <- struct{}{}
		}()

		return e.Next()
	})

	// updating app1 settings should trigger a reload in app2
	app1.Settings().SuperuserIPs = []string{"127.0.0.1"}
	if err := app1.Save(app1.Settings()); err != nil {
		t.Fatal(err)
	}

	// wait for the event
	select {
	case <-timeout:
		t.Fatal("app2 reload event timeout")
	case <-done:
		// ready
	}

	if app1Total := testEvents.Get(app1); app1Total != 1 {
		t.Fatalf("Expected 1 app1 event, got %d", app1Total)
	}

	if app2Total := testEvents.Get(app2); app2Total != 1 {
		t.Fatalf("Expected 1 app2 event, got %d", app2Total)
	}

	app2SuperuserIPs := app2.Settings().SuperuserIPs
	if len(app2SuperuserIPs) != 1 || app2SuperuserIPs[0] != "127.0.0.1" {
		t.Fatalf("Expected exactly 127.0.0.1 superuser IP in app2 settings event, got %v", app2SuperuserIPs)
	}
}

func TestNotifyWatcher_CollectionsUpdate(t *testing.T) {
	t.Parallel()

	tmpDir, err := os.MkdirTemp("", "pb_notify_test*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// popgres: both instances share the same database
	db, err := tests.NewTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer tests.DropTestDatabase(db.Name)

	app1 := core.NewBaseApp(core.BaseAppConfig{
		DataDir:     tmpDir,
		DatabaseURL: db.URL,
	})
	if err := app1.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app1.ClearBootstrap()

	app2 := core.NewBaseApp(core.BaseAppConfig{
		DataDir:     tmpDir,
		DatabaseURL: db.URL,
	})
	if err := app2.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app2.ClearBootstrap()

	select {
	case <-app2.Cluster().Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("app2 cluster LISTEN timeout")
	}

	// popgres: the concurrent and nonconcurrent builders share the same pool
	testQueries := store.New[string, []string](nil)
	logQuery := func(sql string) {
		if strings.Contains(sql, `FROM "_collections"`) {
			testQueries.SetFunc("collections", func(old []string) []string {
				return append(old, sql)
			})
		}
	}
	app2.ConcurrentDB().(*dbx.DB).QueryLogFunc = func(ctx context.Context, t time.Duration, sql string, rows *sql.Rows, err error) {
		logQuery(sql)
	}
	app2.ConcurrentDB().(*dbx.DB).ExecLogFunc = func(ctx context.Context, t time.Duration, sql string, result sql.Result, err error) {
		logQuery(sql)
	}

	// create/update/delete app1 collections should trigger an immediate reload in app2
	// (the changes received while reloading are coalesced into a single extra reload)
	dummyCollection := core.NewBaseCollection("test")
	if err := app1.Save(dummyCollection); err != nil {
		t.Fatal(err)
	}
	dummyCollection.Fields.Add(&core.TextField{Name: "test"})
	if err := app1.Save(dummyCollection); err != nil {
		t.Fatal(err)
	}
	if err := app1.Delete(dummyCollection); err != nil {
		t.Fatal(err)
	}

	// currently there is no hook for the collections cache reload so we poll instead
	deadline := time.Now().Add(1 * time.Second)
	for len(testQueries.Get("collections")) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	// wait a little longer to ensure that there are no other reloads
	time.Sleep(200 * time.Millisecond)

	// popgres: at most one reload per change
	collectionsQueries := testQueries.Get("collections")
	if len(collectionsQueries) < 1 || len(collectionsQueries) > 3 {
		t.Fatalf("Expected 1-3 collections queries, got %d (%v)", len(collectionsQueries), collectionsQueries)
	}

	expectedQuery := `SELECT {{_collections}}.* FROM "_collections" ORDER BY "created" ASC, "id" ASC`
	for _, q := range collectionsQueries {
		if q != expectedQuery {
			t.Fatalf("Expected query\n%s\ngot\n%s", expectedQuery, q)
		}
	}

	if _, err := app2.FindCachedCollectionByNameOrId("test"); err == nil {
		t.Fatal("Expected the deleted collection to be removed from the app2 cache")
	}

	// a collection created in app1 should be visible in app2
	visible := core.NewBaseCollection("test_visible")
	if err := app1.Save(visible); err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(1 * time.Second)
	for {
		if _, err := app2.FindCachedCollectionByNameOrId("test_visible"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Expected the new app1 collection to be cached in app2 in less than 1s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFindCachedCollectionClusterFallback(t *testing.T) {
	t.Parallel()

	db, err := tests.NewTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer tests.DropTestDatabase(db.Name)

	newApp := func(clusterMode bool) *core.BaseApp {
		app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir(), DatabaseURL: db.URL, ClusterMode: clusterMode})
		if err := app.Bootstrap(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { app.ClearBootstrap() })
		return app
	}

	app1 := newApp(true)
	app2 := newApp(true)
	app3 := newApp(false)

	// stop the listeners so that the caches are not reloaded
	app2.Cluster().Stop()
	app3.Cluster().Stop()

	if err := app1.Save(core.NewBaseCollection("fallback_test")); err != nil {
		t.Fatal(err)
	}

	// cluster mode: loaded from the database
	if _, err := app2.FindCachedCollectionByNameOrId("FALLBACK_test"); err != nil {
		t.Fatalf("Expected the collection to be found in cluster mode, got %v", err)
	}

	// single node: the cache only
	if _, err := app3.FindCachedCollectionByNameOrId("fallback_test"); err == nil {
		t.Fatal("Expected the collection to be missing from the stale cache without cluster mode")
	}
}
