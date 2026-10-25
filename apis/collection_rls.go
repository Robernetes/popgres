package apis

import (
	"net/http"

	"github.com/Robernetes/popgres/core"
)

// collectionRLS returns the RLS policies compiled from the collection API rules
// and whether they are applied in the database.
func collectionRLS(e *core.RequestEvent) error {
	collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || collection == nil {
		return e.NotFoundError("", err)
	}

	compiled, err := core.CompileRLS(e.App, collection)
	if err != nil {
		return e.InternalServerError("Failed to compile the collection RLS policies.", err)
	}

	applyStatus, err := core.RLSApplyStatus(e.App, compiled)
	if err != nil {
		return e.InternalServerError("Failed to load the RLS state.", err)
	}

	return e.JSON(http.StatusOK, struct {
		*core.RLSCollection
		ApplyStatus string `json:"applyStatus"`
		Strict      bool   `json:"strict"`
	}{
		RLSCollection: compiled,
		ApplyStatus:   applyStatus,
		Strict:        e.App.IsRLSStrict(),
	})
}
