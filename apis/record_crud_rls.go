package apis

import (
	"github.com/Robernetes/popgres/core"
)

// isRLSStrictRequest reports whether the main record query of the request
// must be executed as the RLS API role (see core.BaseAppConfig.RLSStrict).
//
// The superusers bypass the rules and the rules that can't be translated to
// RLS policies are still enforced only by the API.
func isRLSStrictRequest(e *core.RequestEvent, requestInfo *core.RequestInfo, collection *core.Collection, rule string) bool {
	if !e.App.IsRLSStrict() || requestInfo.HasSuperuserAuth() {
		return false
	}

	if requestInfo.Auth != nil && requestInfo.Auth.IsSuperuser() {
		return false
	}

	return core.RLSRuleTranslatable(e.App, collection, rule)
}
