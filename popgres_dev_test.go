package popgres

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Robernetes/popgres/apis"
	"github.com/Robernetes/popgres/core"
)

func TestShouldUseDevPostgres(t *testing.T) {
	// the env of this package has a DATABASE_URL (see TestMain)
	t.Setenv(core.DatabaseURLEnv, "")

	scenarios := []struct {
		isDev       bool
		databaseURL string
		envURL      string
		expected    bool
	}{
		{false, "", "", false},
		{true, "", "", true},
		{true, "postgres://localhost/test", "", false},
		{true, "", "postgres://localhost/test", false},
	}

	for i, s := range scenarios {
		t.Setenv(core.DatabaseURLEnv, s.envURL)

		if result := shouldUseDevPostgres(s.isDev, s.databaseURL); result != s.expected {
			t.Fatalf("[%d] Expected %v, got %v", i, s.expected, result)
		}
	}
}

// startDevApp starts the embedded Postgres and the app of the specified data
// directory (the same as "popgres serve --dev") and returns the time until the
// health endpoint responded with 200 and a stop function.
func startDevApp(t *testing.T, dataDir string) (time.Duration, func()) {
	t.Helper()

	start := time.Now()

	dp, err := newDevPostgres(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := dp.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("embedded Postgres started: %v", time.Since(start))

	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:     dataDir,
		DatabaseURL: dp.URL(),
		IsDev:       false,
	})
	if err := app.Bootstrap(); err != nil {
		_ = dp.Stop()
		t.Fatal(err)
	}

	t.Logf("app bootstrapped: %v", time.Since(start))

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected health status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	elapsed := time.Since(start)

	return elapsed, func() {
		if err := app.ClearBootstrap(); err != nil {
			t.Errorf("Failed to clear the app bootstrap: %v", err)
		}
		if err := dp.Stop(); err != nil {
			t.Errorf("Failed to stop the embedded Postgres: %v", err)
		}
	}
}

// TestDevPostgres starts the embedded Postgres of the dev mode.
//
// It downloads the Postgres binaries on the first run (when they are not
// cached yet), so it runs only with POPGRES_TEST_EMBEDDED=1.
func TestDevPostgres(t *testing.T) {
	if os.Getenv("POPGRES_TEST_EMBEDDED") == "" {
		t.Skip("set POPGRES_TEST_EMBEDDED=1 to run the embedded Postgres test")
	}

	dataDir := t.TempDir()

	// first start (downloads the binaries if they are not cached)
	first, stop := startDevApp(t, dataDir)
	stop()
	t.Logf("first start: %v", first)

	if first > 30*time.Second {
		t.Fatalf("Expected the first start in less than 30s, got %v", first)
	}

	// start with the cached binaries and the existing data
	second, stop := startDevApp(t, dataDir)
	stop()
	t.Logf("second start: %v", second)

	// note: "pg_ctl start" alone takes about 2s on Windows (process creation)
	maxSecond := 3 * time.Second
	if runtime.GOOS == "windows" {
		maxSecond = 5 * time.Second
	}

	if second > maxSecond {
		t.Fatalf("Expected the second start in less than %v, got %v", maxSecond, second)
	}

	// reset
	if err := ResetDevPostgres(dataDir); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, DevPostgresDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Expected the %s directory to be empty after the reset, got %d entries", DevPostgresDirName, len(entries))
	}
}

func TestResetDevPostgresRunning(t *testing.T) {
	dataDir := t.TempDir()

	pidDir := filepath.Join(dataDir, DevPostgresDirName, "data")
	if err := os.MkdirAll(pidDir, os.ModePerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "postmaster.pid"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ResetDevPostgres(dataDir); err == nil {
		t.Fatal("Expected an error for a running embedded Postgres")
	}

	// missing directory
	if err := ResetDevPostgres(t.TempDir()); err != nil {
		t.Fatalf("Expected no error for a missing directory, got %v", err)
	}
}
