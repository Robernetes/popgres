package tests

import (
	"errors"
	"fmt"

	"github.com/Robernetes/popgres/core"
	"github.com/pocketbase/dbx"
)

// errRLSRollback is used to always rollback the RunWithRLSRole transaction.
var errRLSRollback = errors.New("rls test rollback")

// RunWithRLSRole executes fn in a transaction (always rolled back) as the
// provided external RLS role (core.RLSAnonRole or core.RLSAuthenticatedRole).
//
// The connection role of the tests is a superuser (locally) or the tables owner
// (in the CI) and both bypass the RLS policies, so the policies must be
// checked only after switching the role. Before calling fn it verifies that
// the current role is the expected one and that it can't bypass RLS.
//
// If token is not empty, it is verified and set with popgres_rls.set_claims.
func RunWithRLSRole(app core.App, role string, token string, fn func(txApp core.App) error) error {
	if role != core.RLSAnonRole && role != core.RLSAuthenticatedRole {
		return fmt.Errorf("unsupported RLS role %q", role)
	}

	err := app.RunInTransaction(func(txApp core.App) error {
		_, err := txApp.DB().NewQuery("SELECT set_config('role', {:role}, true)").
			Bind(dbx.Params{"role": role}).
			Execute()
		if err != nil {
			return err
		}

		var current string
		var isSuperuser, canBypass bool
		err = txApp.DB().NewQuery("SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user").
			Row(&current, &isSuperuser, &canBypass)
		if err != nil {
			return err
		}
		if current != role {
			return fmt.Errorf("expected the current role %q, got %q", role, current)
		}
		if isSuperuser || canBypass {
			return fmt.Errorf("the role %q can bypass RLS (superuser: %v, bypassrls: %v)", role, isSuperuser, canBypass)
		}

		if token != "" {
			_, err := txApp.DB().NewQuery("SELECT " + core.RLSSchema + ".set_claims({:token}, true)").
				Bind(dbx.Params{"token": token}).
				Execute()
			if err != nil {
				return err
			}
		}

		if err := fn(txApp); err != nil {
			return err
		}

		return errRLSRollback
	})

	if errors.Is(err, errRLSRollback) {
		return nil
	}

	return err
}
