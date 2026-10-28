package popgres

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	devPostgresRepositoryURL = "https://repo1.maven.org/maven2"

	maxDevPostgresJarSize = 200 << 20

	devPostgresDownloadTimeout = 10 * time.Minute
)

// devPostgresJarSHA256 are the SHA-256 checksums of the zonky embedded Postgres
// binaries jars of devPostgresVersion by platform ("<os>-<arch>" as named by zonky).
//
// The binaries of these platforms are downloaded and verified by popgres instead of the
// embedded-postgres library, which verifies them only with a checksum downloaded from
// the same server (and skips the verification if the checksum is not found).
var devPostgresJarSHA256 = map[string]string{
	"linux-amd64":    "1974c3a59c3ae010e9818e81637e425aad94eaac26e4ef533b42e081fe7ca345",
	"linux-arm64v8":  "1fb555a6602c13a113ab1f0cc9224d599948e85e51ac5f001393605505eaf78e",
	"darwin-amd64":   "cb68c57232945f5e74db946e6077d70cec56bfed35959b21acc750cc80ca0977",
	"darwin-arm64v8": "a18ec9ec6a905e04f5a08a8f7870bddadd635feb54dee332a3daf80fada3cb1a",
	"windows-amd64":  "e1e2bb484f775c0b70fbde39c69451a7a4925629a85d622f557fe6f60de37ba1",
}

// devPostgresPlatform returns the zonky platform name of the current system
// or an empty string if popgres has no checksum for it (eg. Alpine Linux).
func devPostgresPlatform() string {
	arch := runtime.GOARCH
	if arch == "arm64" && runtime.GOOS != "windows" {
		arch = "arm64v8"
	}

	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/etc/alpine-release"); err == nil {
			return ""
		}
	}

	platform := runtime.GOOS + "-" + arch
	if _, ok := devPostgresJarSHA256[platform]; !ok {
		return ""
	}

	return platform
}

// devPostgresCacheDir returns the directory of the verified binaries archives
// (shared by all popgres data directories of the user).
func devPostgresCacheDir(fallback string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return fallback
	}

	return filepath.Join(dir, "popgres", "embedded-postgres")
}

// devPostgresArchiveName returns the name (without extension) of the binaries
// archive of the platform, the same as the embedded-postgres library cache.
func devPostgresArchiveName(platform string) string {
	return "embedded-postgres-binaries-" + platform + "-" + string(devPostgresVersion)
}

// ensureDevPostgresBinaries downloads the binaries jar of the platform from repositoryURL,
// verifies its checksum and stores its binaries archive (.txz) in cacheDir, where the
// embedded-postgres library finds it instead of downloading it again.
//
// It is no-op if the archive is already in cacheDir.
func ensureDevPostgresBinaries(repositoryURL string, cacheDir string, platform string, checksum string) error {
	name := devPostgresArchiveName(platform)

	archive := filepath.Join(cacheDir, name+".txz")
	if _, err := os.Stat(archive); err == nil {
		return nil
	}

	jarURL := fmt.Sprintf(
		"%s/io/zonky/test/postgres/embedded-postgres-binaries-%s/%s/%s.jar",
		strings.TrimSuffix(repositoryURL, "/"),
		platform,
		devPostgresVersion,
		name,
	)

	client := &http.Client{Timeout: devPostgresDownloadTimeout}

	resp, err := client.Get(jarURL)
	if err != nil {
		return fmt.Errorf("failed to download the Postgres binaries: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download the Postgres binaries from %s: status %d", jarURL, resp.StatusCode)
	}

	jar, err := io.ReadAll(io.LimitReader(resp.Body, maxDevPostgresJarSize+1))
	if err != nil {
		return fmt.Errorf("failed to download the Postgres binaries: %w", err)
	}
	if len(jar) > maxDevPostgresJarSize {
		return errors.New("the Postgres binaries download is too large")
	}

	sum := sha256.Sum256(jar)
	if got := hex.EncodeToString(sum[:]); got != checksum {
		return fmt.Errorf("the checksum of the Postgres binaries downloaded from %s doesn't match (expected %s, got %s)", jarURL, checksum, got)
	}

	zr, err := zip.NewReader(bytes.NewReader(jar), int64(len(jar)))
	if err != nil {
		return fmt.Errorf("invalid Postgres binaries jar: %w", err)
	}

	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasSuffix(f.Name, ".txz") {
			continue
		}

		return writeDevPostgresArchive(f, archive)
	}

	return errors.New("the Postgres binaries jar doesn't contain a .txz archive")
}

// writeDevPostgresArchive extracts the zip file to path (through a temporary file,
// so that an interrupted write doesn't leave an incomplete archive).
func writeDevPostgresArchive(f *zip.File, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
		return err
	}

	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, io.LimitReader(src, maxDevPostgresJarSize)); err != nil {
		tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}
