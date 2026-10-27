package apis

import (
	"net/http"
	"time"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tools/router"
	validation "github.com/pocketbase/ozzo-validation/v4"
)

// popgres: the tokens of the direct SQL clients (see docs/RLS.md).

// bindRLSTokenApi registers the RLS token api endpoint.
func bindRLSTokenApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	rg.POST("/rls/token", rlsToken).Bind(RequireAuth())
}

// rlsToken returns a short-lived RLS token of the authenticated record
// (for popgres_rls.set_claims, it is not accepted by the API).
func rlsToken(e *core.RequestEvent) error {
	// extra check for just in case the handler is called in a different context
	if e.Auth == nil {
		return e.UnauthorizedError("Missing auth context.", nil)
	}

	form := &rlsTokenForm{}
	if err := e.BindBody(form); err != nil {
		return e.BadRequestError("An error occurred while loading the submitted data.", err)
	}
	if err := form.validate(); err != nil {
		return e.BadRequestError("An error occurred while validating the submitted data.", err)
	}

	token, err := e.Auth.NewRLSToken(time.Duration(form.Duration) * time.Second)
	if err != nil {
		return e.InternalServerError("Failed to generate the RLS token.", err)
	}

	return e.JSON(http.StatusOK, map[string]string{"token": token})
}

type rlsTokenForm struct {
	// Duration is the optional token duration in seconds
	// (default to core.DefaultRLSTokenDuration).
	Duration int64 `form:"duration" json:"duration"`
}

func (form *rlsTokenForm) validate() error {
	return validation.ValidateStruct(form,
		validation.Field(
			&form.Duration,
			validation.Min(int64(60)),
			validation.Max(int64(core.MaxRLSTokenDuration/time.Second)),
		),
	)
}
