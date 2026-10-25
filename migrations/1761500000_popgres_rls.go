package migrations

import (
	"github.com/Robernetes/popgres/core"
)

// popgres row level security.
//
// Creates the external roles (popgres_anon and popgres_authenticated), the
// popgres schema with the auth token functions and the policies compiled from
// the collection API rules. The app connection role (the tables owner) is not
// affected by the policies.
func init() {
	core.SystemMigrations.Register(func(txApp core.App) error {
		return core.InstallRLS(txApp)
	}, func(txApp core.App) error {
		return core.UninstallRLS(txApp)
	})
}
