package popgres

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Robernetes/popgres/cmd"
	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tools/list"
	"github.com/Robernetes/popgres/tools/osutils"
	"github.com/Robernetes/popgres/tools/routine"
	"github.com/spf13/cobra"

	_ "github.com/Robernetes/popgres/migrations"
)

var _ core.App = (*PocketBase)(nil)

// Version of PocketBase
var Version = "(untracked)"

// PocketBase defines a PocketBase app launcher.
//
// It implements [core.App] via embedding and all of the app interface methods
// could be accessed directly through the instance (eg. PocketBase.DataDir()).
type PocketBase struct {
	core.App

	devFlag           bool
	dataDirFlag       string
	encryptionEnvFlag string
	queryTimeout      int
	hideStartBanner   bool

	// devPostgres is the embedded Postgres of the dev mode (if used)
	devPostgres *devPostgres

	// RootCmd is the main console command
	RootCmd *cobra.Command
}

// Config is the PocketBase initialization config struct.
type Config struct {
	// hide the default console server info on app startup
	HideStartBanner bool

	// optional default values for the console flags
	DefaultDev           bool
	DefaultDataDir       string // if not set, it will fallback to "./popgres_data"
	DefaultEncryptionEnv string
	DefaultQueryTimeout  time.Duration // default to core.DefaultQueryTimeout (in seconds)

	// optional DB configurations
	DatabaseURL      string             // default to the DATABASE_URL env variable
	LogsDatabaseURL  string             // default to the POPGRES_LOGS_URL env variable (or DatabaseURL)
	DataMaxOpenConns int                // default to core.DefaultDataMaxOpenConns
	DataMaxIdleConns int                // default to core.DefaultDataMaxIdleConns
	AuxMaxOpenConns  int                // default to core.DefaultAuxMaxOpenConns
	AuxMaxIdleConns  int                // default to core.DefaultAuxMaxIdleConns
	DBConnect        core.DBConnectFunc // default to core.DefaultDBConnect
}

// New creates a new PocketBase instance with the default configuration.
// Use [NewWithConfig] if you want to provide a custom configuration.
//
// Note that the application will not be initialized/bootstrapped yet,
// aka. DB connections, migrations, app settings, etc. will not be accessible.
// Everything will be initialized when [PocketBase.Start] is executed.
// If you want to initialize the application before calling [PocketBase.Start],
// then you'll have to manually call [PocketBase.Bootstrap].
func New() *PocketBase {
	_, isUsingGoRun := inspectRuntime()

	return NewWithConfig(Config{
		DefaultDev: isUsingGoRun,
	})
}

// NewWithConfig creates a new PocketBase instance with the provided config.
//
// Note that the application will not be initialized/bootstrapped yet,
// aka. DB connections, migrations, app settings, etc. will not be accessible.
// Everything will be initialized when [PocketBase.Start] is executed.
// If you want to initialize the application before calling [PocketBase.Start],
// then you'll have to manually call [PocketBase.Bootstrap].
func NewWithConfig(config Config) *PocketBase {
	// initialize a default data directory based on the executable baseDir
	if config.DefaultDataDir == "" {
		baseDir, _ := inspectRuntime()
		config.DefaultDataDir = filepath.Join(baseDir, "popgres_data")
	}

	if config.DefaultQueryTimeout == 0 {
		config.DefaultQueryTimeout = core.DefaultQueryTimeout
	}

	executableName := filepath.Base(os.Args[0])

	pb := &PocketBase{
		RootCmd: &cobra.Command{
			Use:     executableName,
			Short:   executableName + " CLI",
			Version: Version,
			FParseErrWhitelist: cobra.FParseErrWhitelist{
				UnknownFlags: true,
			},
			// no need to provide the default cobra completion command
			CompletionOptions: cobra.CompletionOptions{
				DisableDefaultCmd: true,
			},
		},
		devFlag:           config.DefaultDev,
		dataDirFlag:       config.DefaultDataDir,
		encryptionEnvFlag: config.DefaultEncryptionEnv,
		hideStartBanner:   config.HideStartBanner,
	}

	// don't write command errors to the stderr because the error is
	// propagated back to the app.Start() and could result in duplication
	pb.RootCmd.SetErr(&nopWrite{})

	// parse base flags
	// (errors are ignored, since the full flags parsing happens on Execute())
	pb.eagerParseFlags(&config)

	// popgres: the embedded Postgres of the dev mode (started before the app bootstrap)
	if shouldUseDevPostgres(pb.devFlag, config.DatabaseURL) {
		if dp, err := newDevPostgres(pb.dataDirFlag); err == nil {
			pb.devPostgres = dp
			config.DatabaseURL = dp.URL()
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
	}

	// initialize the app instance
	pb.App = core.NewBaseApp(core.BaseAppConfig{
		IsDev:            pb.devFlag,
		DataDir:          pb.dataDirFlag,
		EncryptionEnv:    pb.encryptionEnvFlag,
		QueryTimeout:     time.Duration(pb.queryTimeout) * time.Second,
		DataMaxOpenConns: config.DataMaxOpenConns,
		DataMaxIdleConns: config.DataMaxIdleConns,
		AuxMaxOpenConns:  config.AuxMaxOpenConns,
		AuxMaxIdleConns:  config.AuxMaxIdleConns,
		DBConnect:        config.DBConnect,
		DatabaseURL:      config.DatabaseURL,
		LogsDatabaseURL:  config.LogsDatabaseURL,
	})

	// hide the default help command (allow only `--help` flag)
	pb.RootCmd.SetHelpCommand(&cobra.Command{Hidden: true})

	return pb
}

// Start starts the application, aka. registers the default system
// commands (serve, superuser, version) and executes pb.RootCmd.
func (pb *PocketBase) Start() error {
	// register system commands
	pb.RootCmd.AddCommand(cmd.NewSuperuserCommand(pb))
	pb.RootCmd.AddCommand(cmd.NewServeCommand(pb, !pb.hideStartBanner))
	pb.RootCmd.AddCommand(cmd.NewRestoreCommand(pb))
	pb.RootCmd.AddCommand(newDevCommand(pb))

	return pb.Execute()
}

// Execute initializes the application (if not already) and executes
// the pb.RootCmd with graceful shutdown support.
//
// This method differs from pb.Start() by not registering the default
// system commands!
func (pb *PocketBase) Execute() error {
	if !pb.skipBootstrap() {
		if pb.devPostgres != nil {
			if err := pb.startDevPostgres(); err != nil {
				return err
			}
		}

		if err := pb.Bootstrap(); err != nil {
			return errors.Join(err, pb.stopDevPostgres())
		}
	}

	execCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)

	// listen for interrupt signal to gracefully shutdown the application
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	// execute the root command
	go func() {
		execCh <- routine.SafeWrap(pb.RootCmd.Execute)()
	}()

	// wait for either an OS signal or the command to complete
	var execErr error
	select {
	case <-sigCh:
	case execErr = <-execCh:
	}

	signal.Stop(sigCh)

	// trigger cleanups
	//
	// @todo consider skipping and just call the finalizer in case OnTerminate was already invoked manually?
	event := new(core.TerminateEvent)
	event.App = pb
	return pb.OnTerminate().Trigger(event, func(e *core.TerminateEvent) error {
		return errors.Join(e.App.ClearBootstrap(), pb.stopDevPostgres(), execErr)
	})
}

func (pb *PocketBase) startDevPostgres() error {
	if !pb.hideStartBanner {
		fmt.Println("Starting the embedded Postgres of the dev mode (the first start downloads it)...")
	}

	if err := pb.devPostgres.Start(); err != nil {
		return err
	}

	// the embedded binaries don't include pg_dump
	if !pb.hideStartBanner && os.Getenv(core.PgDumpEnv) == "" {
		if _, err := exec.LookPath("pg_dump"); err != nil {
			fmt.Println("The embedded Postgres doesn't include pg_dump, the backups are not available (install the Postgres client tools to enable them).")
		}
	}

	return nil
}

func (pb *PocketBase) stopDevPostgres() error {
	if pb.devPostgres == nil {
		return nil
	}

	return pb.devPostgres.Stop()
}

// eagerParseFlags parses the global app flags before calling pb.RootCmd.Execute().
// so we can have all PocketBase flags ready for use on initialization.
func (pb *PocketBase) eagerParseFlags(config *Config) error {
	pb.RootCmd.PersistentFlags().StringVar(
		&pb.dataDirFlag,
		"dir",
		config.DefaultDataDir,
		"the popgres data directory",
	)

	pb.RootCmd.PersistentFlags().StringVar(
		&pb.encryptionEnvFlag,
		"encryptionEnv",
		config.DefaultEncryptionEnv,
		"the env variable whose value of 32 characters will be used \nas encryption key for the app settings (default none)",
	)

	pb.RootCmd.PersistentFlags().BoolVar(
		&pb.devFlag,
		"dev",
		config.DefaultDev,
		"enable dev mode, aka. printing logs and sql statements to the console\n(without a DATABASE_URL it starts an embedded Postgres in the data directory)",
	)

	pb.RootCmd.PersistentFlags().IntVar(
		&pb.queryTimeout,
		"queryTimeout",
		int(config.DefaultQueryTimeout.Seconds()),
		"the default SELECT queries timeout in seconds",
	)

	return pb.RootCmd.ParseFlags(os.Args[1:])
}

// skipBootstrap eagerly checks if the app should skip the bootstrap process:
// - already bootstrapped
// - is unknown command
// - is the default help command
// - is the default version command
//
// https://github.com/pocketbase/pocketbase/issues/404
// https://github.com/pocketbase/pocketbase/discussions/1267
func (pb *PocketBase) skipBootstrap() bool {
	flags := []string{
		"-h",
		"--help",
		"-v",
		"--version",
	}

	if pb.IsBootstrapped() {
		return true // already bootstrapped
	}

	// popgres: the dev commands don't need the database
	if isDevCommand(pb.RootCmd) {
		return true
	}

	cmd, _, err := pb.RootCmd.Find(os.Args[1:])
	if err != nil {
		return true // unknown command
	}

	for _, arg := range os.Args {
		if !list.ExistInSlice(arg, flags) {
			continue
		}

		// ensure that there is no user defined flag with the same name/shorthand
		trimmed := strings.TrimLeft(arg, "-")
		if len(trimmed) > 1 && cmd.Flags().Lookup(trimmed) == nil {
			return true
		}
		if len(trimmed) == 1 && cmd.Flags().ShorthandLookup(trimmed) == nil {
			return true
		}
	}

	return false
}

// inspectRuntime tries to find the base executable directory and how it was run.
//
// note: we are using os.Args[0] and not os.Executable() since it could
// break existing aliased binaries (eg. the community maintained homebrew package)
func inspectRuntime() (baseDir string, withGoRun bool) {
	if osutils.IsProbablyGoRun() {
		// probably ran with go run
		withGoRun = true
		baseDir, _ = os.Getwd()
	} else {
		// probably ran with go build
		withGoRun = false
		baseDir = filepath.Dir(os.Args[0])
	}
	return
}

var _ io.Writer = (*nopWrite)(nil)

type nopWrite struct{}

func (w *nopWrite) Write(p []byte) (n int, err error) {
	return
}
