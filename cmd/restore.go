package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Robernetes/popgres/core"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// NewRestoreCommand creates and returns new command for restoring a popgres backup
// (created with the dashboard, the backups API or app.CreateBackup).
//
// The app must not be running (in any instance) while the backup is restored.
func NewRestoreCommand(app core.App) *cobra.Command {
	var confirmed bool

	command := &cobra.Command{
		Use:   "restore <backup>",
		Args:  cobra.ExactArgs(1),
		Short: "Restores a backup (a local zip file path or the name of a backup in the backups storage)",
		Long: "Restores a backup (a local zip file path or the name of a backup in the backups storage).\n\n" +
			"The database objects of the backup are dropped and recreated with pg_restore and the\n" +
			"backup files (eg. the storage files) replace the local ones. Stop all app instances first.",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			urls, ok := app.(interface {
				DatabaseURL() string
				LogsDatabaseURL() string
			})
			if !ok {
				return errors.New("the app doesn't expose its database connection strings")
			}

			if !confirmed {
				color.Yellow("This will replace the current database objects and data files with the ones from %q.", args[0])
				color.Yellow("Stop all app instances first and run the command again with --yes to confirm.")
				return errors.New("restore not confirmed")
			}

			zipPath, cleanup, err := resolveBackupArchive(app, args[0])
			if err != nil {
				return err
			}
			defer cleanup()

			err = core.RestoreBackupArchive(command.Context(), zipPath, urls.DatabaseURL(), urls.LogsDatabaseURL(), app.DataDir())
			if err != nil {
				return fmt.Errorf("failed to restore the backup: %w", err)
			}

			color.Green("Successfully restored backup %q.", args[0])

			return nil
		},
	}

	command.Flags().BoolVar(&confirmed, "yes", false, "confirm the restore")

	return command
}

// resolveBackupArchive returns the local path of the specified backup
// (downloading it from the backups storage if it is not a local file).
func resolveBackupArchive(app core.App, backup string) (string, func(), error) {
	if info, err := os.Stat(backup); err == nil && !info.IsDir() {
		return backup, func() {}, nil
	}

	fsys, err := app.NewBackupsFilesystem()
	if err != nil {
		return "", nil, err
	}
	defer fsys.Close()

	reader, err := fsys.GetReader(backup)
	if err != nil {
		return "", nil, fmt.Errorf("backup %q not found in the backups storage: %w", backup, err)
	}
	defer reader.Close()

	tempFile, err := os.CreateTemp("", "popgres_backup_*.zip")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_ = os.Remove(tempFile.Name())
	}

	if _, err := io.Copy(tempFile, reader); err != nil {
		tempFile.Close()
		cleanup()
		return "", nil, err
	}

	if err := tempFile.Close(); err != nil {
		cleanup()
		return "", nil, err
	}

	return tempFile.Name(), cleanup, nil
}
