package popgres

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/Robernetes/popgres/core"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/spf13/cobra"
)

// The embedded Postgres of the dev mode.
//
// "popgres serve --dev" without a DATABASE_URL starts a local Postgres server
// (the zonky binaries downloaded and cached on the first start) with the data
// in "<dataDir>/.pg". It is intended only for development.

const (
	// DevPostgresDirName is the name of the data directory subfolder of the embedded Postgres.
	DevPostgresDirName = ".pg"

	devPostgresVersion  = embeddedpostgres.V18
	devPostgresDatabase = "popgres"

	// note: the server listens only on localhost
	devPostgresUsername = "popgres"
	devPostgresPassword = "popgres"

	devPostgresStartTimeout = 2 * time.Minute
)

// devPostgres is an embedded Postgres server of the dev mode.
type devPostgres struct {
	server *embeddedpostgres.EmbeddedPostgres
	dir    string
	url    string
	log    *os.File
}

// shouldUseDevPostgres reports whether the embedded Postgres should be used
// (the dev mode without an explicit database connection string).
func shouldUseDevPostgres(isDev bool, databaseURL string) bool {
	return isDev && databaseURL == "" && os.Getenv(core.DatabaseURLEnv) == ""
}

// newDevPostgres initializes (but doesn't start) the embedded Postgres
// of the specified data directory on a free local port.
func newDevPostgres(dataDir string) (*devPostgres, error) {
	port, err := freeLocalPort()
	if err != nil {
		return nil, fmt.Errorf("failed to find a free port for the embedded Postgres: %w", err)
	}

	dir := filepath.Join(dataDir, DevPostgresDirName)

	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(devPostgresUsername, devPostgresPassword),
		Host:     "localhost:" + strconv.Itoa(port),
		Path:     "/" + devPostgresDatabase,
		RawQuery: "sslmode=disable",
	}

	return &devPostgres{dir: dir, url: u.String()}, nil
}

// URL returns the connection string of the embedded Postgres.
func (p *devPostgres) URL() string {
	return p.url
}

// Start starts the embedded Postgres (downloading its binaries on the first start).
func (p *devPostgres) Start() error {
	if err := os.MkdirAll(p.dir, os.ModePerm); err != nil {
		return err
	}

	u, err := url.Parse(p.url)
	if err != nil {
		return err
	}
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil {
		return err
	}

	log, err := os.OpenFile(filepath.Join(p.dir, "postgres.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	p.log = log

	binaries := filepath.Join(p.dir, "bin-"+string(devPostgresVersion))

	config := embeddedpostgres.DefaultConfig().
		Version(devPostgresVersion).
		Port(uint32(port)).
		Database(devPostgresDatabase).
		Username(devPostgresUsername).
		Password(devPostgresPassword).
		Locale("C").
		Encoding("UTF8").
		// the extracted binaries are kept so that the next starts are faster
		BinariesPath(binaries).
		RuntimePath(filepath.Join(p.dir, "runtime")).
		DataPath(filepath.Join(p.dir, "data")).
		StartParameters(map[string]string{
			"wal_level":             "logical",
			"max_replication_slots": "10",
			"max_wal_senders":       "10",
			// faster starts and writes (development only data)
			"shared_buffers":     "32MB",
			"fsync":              "off",
			"synchronous_commit": "off",
			"full_page_writes":   "off",
		}).
		StartTimeout(devPostgresStartTimeout).
		Logger(log)

	// the binaries of the platforms with a popgres checksum are downloaded and verified
	// before the start (the library uses the verified archive of the cache directory)
	if platform := devPostgresPlatform(); platform != "" {
		cacheDir := devPostgresCacheDir(filepath.Join(p.dir, "cache"))
		config = config.CachePath(cacheDir)

		if _, err := os.Stat(filepath.Join(binaries, "bin", "pg_ctl"+executableExtension())); err != nil {
			err := ensureDevPostgresBinaries(devPostgresRepositoryURL, cacheDir, platform, devPostgresJarSHA256[platform])
			if err != nil {
				_ = p.log.Close()
				return err
			}
		}
	}

	// stop a previous server that was not stopped gracefully (eg. the popgres process was killed)
	stopOrphanDevPostgres(binaries, filepath.Join(p.dir, "data"))

	// the library checks for an extracted "bin/pg_ctl" file (without the Windows
	// ".exe" extension) to skip the extraction, so a marker file is created to
	// avoid extracting the binaries again on every start
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(filepath.Join(binaries, "bin", "pg_ctl.exe")); err == nil {
			_ = os.WriteFile(filepath.Join(binaries, "bin", "pg_ctl"), nil, 0o644)
		}
	}

	p.server = embeddedpostgres.NewDatabase(config)

	if err := p.server.Start(); err != nil {
		p.server = nil
		_ = p.log.Close()
		return fmt.Errorf("failed to start the embedded Postgres (see %s): %w", filepath.Join(p.dir, "postgres.log"), err)
	}

	if runtime.GOOS == "windows" {
		_ = os.WriteFile(filepath.Join(binaries, "bin", "pg_ctl"), nil, 0o644)
	}

	// use the pg_dump of the embedded binaries for the backups (if available)
	for env, tool := range map[string]string{core.PgDumpEnv: "pg_dump", core.PgRestoreEnv: "pg_restore"} {
		path := filepath.Join(binaries, "bin", tool+executableExtension())
		if _, err := os.Stat(path); err == nil && os.Getenv(env) == "" {
			os.Setenv(env, path)
		}
	}

	return nil
}

// Stop stops the embedded Postgres (if started).
func (p *devPostgres) Stop() error {
	if p.server == nil {
		return nil
	}

	err := p.server.Stop()
	p.server = nil

	if p.log != nil {
		err = errors.Join(err, p.log.Close())
		p.log = nil
	}

	return err
}

// ResetDevPostgres deletes the embedded Postgres directory content of the specified
// data directory (the downloaded binaries archive is still cached in the user
// cache directory, so the next start doesn't download it again). The server must be stopped.
func ResetDevPostgres(dataDir string) error {
	dir := filepath.Join(dataDir, DevPostgresDirName)

	if _, err := os.Stat(filepath.Join(dir, "data", "postmaster.pid")); err == nil {
		return errors.New("the embedded Postgres seems to be running (stop popgres first)")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}

	return nil
}

// stopOrphanDevPostgres stops (best effort) the server of the data directory
// if it is still running (its postmaster.pid file exists).
func stopOrphanDevPostgres(binaries string, data string) {
	if _, err := os.Stat(filepath.Join(data, "postmaster.pid")); err != nil {
		return
	}

	pgCtl := filepath.Join(binaries, "bin", "pg_ctl"+executableExtension())
	if _, err := os.Stat(pgCtl); err != nil {
		return
	}

	cmd := exec.Command(pgCtl, "stop", "-D", data, "-m", "fast", "-w", "-t", "30")
	_ = cmd.Run()
}

func executableExtension() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// newDevCommand creates the "dev" command with the embedded Postgres subcommands.
func newDevCommand(pb *PocketBase) *cobra.Command {
	command := &cobra.Command{
		Use:   "dev",
		Short: "Manages the embedded Postgres of the dev mode",
	}

	command.AddCommand(&cobra.Command{
		Use:          "reset",
		Short:        "Deletes the embedded Postgres data (" + DevPostgresDirName + " in the data directory)",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			if err := ResetDevPostgres(pb.dataDirFlag); err != nil {
				return err
			}

			fmt.Println("The embedded Postgres data was deleted.")

			return nil
		},
	})

	return command
}

// isDevCommand reports whether the command to execute is a "dev" subcommand
// (they don't need the app bootstrap or the embedded Postgres).
func isDevCommand(root *cobra.Command) bool {
	command, _, err := root.Find(os.Args[1:])
	if err != nil {
		return false
	}

	for c := command; c != nil; c = c.Parent() {
		if c.Name() == "dev" && c.Parent() == root {
			return true
		}
	}

	return false
}

func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil
}
