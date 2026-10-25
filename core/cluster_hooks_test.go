package core_test

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/pocketbase/dbx"
)

func TestClusterClaimCronRun(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	slot := time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC)

	claimed, err := app.Cluster().ClaimCronRun("test", slot)
	if err != nil || !claimed {
		t.Fatalf("Expected the first claim to succeed, got %v (%v)", claimed, err)
	}

	claimed, err = app.Cluster().ClaimCronRun("test", slot)
	if err != nil || claimed {
		t.Fatalf("Expected the same slot to be already claimed, got %v (%v)", claimed, err)
	}

	claimed, err = app.Cluster().ClaimCronRun("test", slot.Add(time.Minute))
	if err != nil || !claimed {
		t.Fatalf("Expected the next slot claim to succeed, got %v (%v)", claimed, err)
	}

	claimed, err = app.Cluster().ClaimCronRun("other", slot)
	if err != nil || !claimed {
		t.Fatalf("Expected the other job claim to succeed, got %v (%v)", claimed, err)
	}
}

func TestClusterCronClaimFunc(t *testing.T) {
	t.Parallel()

	db, err := tests.NewTestDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer tests.DropTestDatabase(db.Name)

	var mu sync.Mutex
	calls := map[string]int{}

	// app instances (nodes) sharing the same database
	apps := make([]*core.BaseApp, 3)
	for i := range apps {
		apps[i] = core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir(), DatabaseURL: db.URL})
		if err := apps[i].Bootstrap(); err != nil {
			t.Fatal(err)
		}
		defer apps[i].ClearBootstrap()

		apps[i].Cron().SetInterval(500 * time.Millisecond)
		apps[i].Cron().MustAdd("test", "* * * * *", func() {
			mu.Lock()
			calls[apps[i].Cluster().NodeId()]++
			mu.Unlock()
		})
	}

	// start the cron tickers of all nodes at the same time
	for _, app := range apps {
		app.Cron().Start()
	}

	// wait for at least 4 ticks
	time.Sleep(2700 * time.Millisecond)

	for _, app := range apps {
		app.Cron().Stop()
	}

	// wait for the last claims to complete
	time.Sleep(300 * time.Millisecond)

	var slots []time.Time
	err = apps[0].DB().Select("slot").
		From("_cron_runs").
		Where(dbx.HashExp{"job_id": "test"}).
		OrderBy("slot").
		Column(&slots)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()

	var total int
	for _, n := range calls {
		total += n
	}

	if len(slots) < 4 {
		t.Fatalf("Expected at least 4 claimed slots, got %d", len(slots))
	}

	if total != len(slots) {
		t.Fatalf("Expected the job to run once per slot (%d), got %d runs (%v)", len(slots), total, calls)
	}
}

func TestClusterDeleteExpired(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	queries := []string{
		"INSERT INTO {{_cron_runs}} ([[job_id]], [[slot]], [[node_id]], [[created]]) VALUES ('old', now(), 'n1', now() - interval '25 hours')",
		"INSERT INTO {{_cron_runs}} ([[job_id]], [[slot]], [[node_id]]) VALUES ('new', now(), 'n1')",
		"INSERT INTO {{_kv_ttl}} ([[key]], [[expires_at]]) VALUES ('old', now() - interval '1 second')",
		"INSERT INTO {{_kv_ttl}} ([[key]], [[expires_at]]) VALUES ('new', now() + interval '1 hour')",
		"INSERT INTO {{_rate_limits}} ([[key]], [[window_start]], [[expires_at]]) VALUES ('old', now(), now() - interval '1 second')",
		"INSERT INTO {{_rate_limits}} ([[key]], [[window_start]], [[expires_at]]) VALUES ('new', now(), now() + interval '1 hour')",
		"INSERT INTO {{_realtime_events}} ([[id]], [[source]], [[op]], [[collection_id]], [[record_id]], [[created]]) VALUES ('old', 'hooks', 'create', 'c', 'r', now() - interval '6 minutes')",
		"INSERT INTO {{_realtime_events}} ([[id]], [[source]], [[op]], [[collection_id]], [[record_id]]) VALUES ('new', 'hooks', 'create', 'c', 'r')",
	}
	for _, q := range queries {
		if _, err := app.DB().NewQuery(q).Execute(); err != nil {
			t.Fatal(err)
		}
	}

	if err := app.Cluster().DeleteExpired(); err != nil {
		t.Fatal(err)
	}

	tables := map[string]string{
		"_cron_runs":       "job_id",
		"_kv_ttl":          "key",
		"_rate_limits":     "key",
		"_realtime_events": "id",
	}
	for table, column := range tables {
		var values []string
		if err := app.DB().Select(column).From(table).Column(&values); err != nil {
			t.Fatal(err)
		}

		if len(values) != 1 || values[0] != "new" {
			t.Fatalf("[%s] Expected only the new row to remain, got %v", table, values)
		}
	}
}

func TestClusterRateLimitHit(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	for i := 1; i <= 4; i++ {
		allowed, err := app.Cluster().RateLimitHit("test", 3, time.Minute)
		if err != nil {
			t.Fatal(err)
		}

		if expected := i <= 3; allowed != expected {
			t.Fatalf("[%d] Expected allowed %v, got %v", i, expected, allowed)
		}
	}

	// the other keys have their own counters
	allowed, err := app.Cluster().RateLimitHit("other", 3, time.Minute)
	if err != nil || !allowed {
		t.Fatalf("Expected the other key to be allowed, got %v (%v)", allowed, err)
	}

	// the window resets after its interval
	if allowed, err := app.Cluster().RateLimitHit("short", 1, 200*time.Millisecond); err != nil || !allowed {
		t.Fatalf("Expected the first short hit to be allowed, got %v (%v)", allowed, err)
	}
	if allowed, err := app.Cluster().RateLimitHit("short", 1, 200*time.Millisecond); err != nil || allowed {
		t.Fatalf("Expected the second short hit to be rejected, got %v (%v)", allowed, err)
	}

	time.Sleep(300 * time.Millisecond)

	if allowed, err := app.Cluster().RateLimitHit("short", 1, 200*time.Millisecond); err != nil || !allowed {
		t.Fatalf("Expected the short hit after the window reset to be allowed, got %v (%v)", allowed, err)
	}
}

func BenchmarkClusterRateLimitHit(b *testing.B) {
	app, err := tests.NewTestApp()
	if err != nil {
		b.Fatal(err)
	}
	defer app.Cleanup()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := app.Cluster().RateLimitHit("bench"+strconv.Itoa(i%100), 1000, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
}
