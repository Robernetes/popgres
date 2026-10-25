package core

import (
	"context"
)

// RestoreBackup is not supported by popgres and always returns [ErrRestoreNotSupported].
//
// PocketBase replaces the pb_data directory and restarts the process, but
// restoring the Postgres database of a running app (or of a cluster with
// other instances using it) is not safe. Stop the app and use the
// "popgres restore <backup>" command (see [RestoreBackupArchive]) instead.
func (app *BaseApp) RestoreBackup(ctx context.Context, name string) error {
	return ErrRestoreNotSupported
}
