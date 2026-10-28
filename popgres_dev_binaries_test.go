package popgres

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDevPostgresJarSHA256(t *testing.T) {
	t.Parallel()

	for platform, checksum := range devPostgresJarSHA256 {
		if len(checksum) != 64 {
			t.Fatalf("Invalid checksum of %s: %q", platform, checksum)
		}
		if _, err := hex.DecodeString(checksum); err != nil {
			t.Fatalf("Invalid checksum of %s: %v", platform, err)
		}
	}
}

func TestEnsureDevPostgresBinaries(t *testing.T) {
	t.Parallel()

	// a fake binaries jar with a .txz archive
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("postgres-test.txz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("test archive")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	jar := buf.Bytes()
	sum := sha256.Sum256(jar)
	checksum := hex.EncodeToString(sum[:])

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/embedded-postgres-binaries-test-x/"+string(devPostgresVersion)+"/"+devPostgresArchiveName("test-x")+".jar") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(jar)
	}))
	defer server.Close()

	archivePath := func(dir string) string {
		return filepath.Join(dir, devPostgresArchiveName("test-x")+".txz")
	}

	t.Run("checksum mismatch", func(t *testing.T) {
		dir := t.TempDir()

		err := ensureDevPostgresBinaries(server.URL, dir, "test-x", strings.Repeat("0", 64))
		if err == nil || !strings.Contains(err.Error(), "doesn't match") {
			t.Fatalf("Expected a checksum error, got %v", err)
		}
		if _, err := os.Stat(archivePath(dir)); err == nil {
			t.Fatal("Expected no archive")
		}
	})

	t.Run("missing jar", func(t *testing.T) {
		dir := t.TempDir()

		err := ensureDevPostgresBinaries(server.URL, dir, "other", checksum)
		if err == nil || !strings.Contains(err.Error(), "status 404") {
			t.Fatalf("Expected a download error, got %v", err)
		}
	})

	t.Run("valid jar", func(t *testing.T) {
		dir := t.TempDir()

		if err := ensureDevPostgresBinaries(server.URL, dir, "test-x", checksum); err != nil {
			t.Fatal(err)
		}

		data, err := os.ReadFile(archivePath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "test archive" {
			t.Fatalf("Expected the archive content, got %q", data)
		}

		// the cached archive is not downloaded again
		before := requests.Load()
		if err := ensureDevPostgresBinaries(server.URL, dir, "test-x", checksum); err != nil {
			t.Fatal(err)
		}
		if requests.Load() != before {
			t.Fatal("Expected no download of the cached archive")
		}
	})
}
