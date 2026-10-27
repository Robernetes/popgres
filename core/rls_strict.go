package core

import (
	"errors"
	"time"

	"github.com/pocketbase/dbx"
)

// rlsStrictTokenDuration is the duration of the RLS tokens generated for the strict mode queries.
const rlsStrictTokenDuration = time.Minute

// IsRLSStrict reports whether the RLS strict mode is enabled
// (the API record queries of the non-superusers are executed as the RLS API role).
func (app *BaseApp) IsRLSStrict() bool {
	return app.config.RLSStrict
}

// RunAsRLSRole executes fn in a new transaction as the RLS API role
// (RLSAPIRole) with the provided auth record (nil for the guests), so that
// the RLS policies apply to the queries of the transaction app.
//
// The optional rule (RLSRuleList or RLSRuleView) restricts the SELECT policies
// to the policy of that collection rule.
func RunAsRLSRole(app App, auth *Record, rule string, fn func(txApp App) error) error {
	var token string

	if auth != nil {
		if auth.IsSuperuser() {
			return errors.New("the superusers bypass the RLS policies")
		}

		var err error
		token, err = auth.NewRLSToken(rlsStrictTokenDuration)
		if err != nil {
			return err
		}
	}

	return app.RunInTransaction(func(txApp App) error {
		_, err := txApp.DB().NewQuery(
			"SELECT set_config('role', {:role}, true), set_config({:tokenSetting}, {:token}, true), set_config({:ruleSetting}, {:rule}, true)",
		).Bind(dbx.Params{
			"role":         RLSAPIRole,
			"tokenSetting": RLSTokenSetting,
			"token":        token,
			"ruleSetting":  RLSRuleSetting,
			"rule":         rule,
		}).Execute()
		if err != nil {
			return err
		}

		return fn(txApp)
	})
}
