package apis

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tools/inflector"
	"github.com/Robernetes/popgres/tools/router"
	"github.com/Robernetes/popgres/tools/search"
	"github.com/Robernetes/popgres/tools/types"
	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
)

// popgres: the pgvector similarity search of the records.

const (
	vectorSearchDefaultK = 10
	vectorSearchMaxK     = 1000
)

// bindRecordVectorSearchApi registers the vector search endpoint.
func bindRecordVectorSearchApi(app core.App, rg *router.RouterGroup[*core.RequestEvent]) {
	// note: the collection rate limit is checked in the handler (the same as the list records)
	rg.POST("/collections/{collection}/vector-search", recordsVectorSearch).Unbind(DefaultRateLimitMiddlewareId)
}

type vectorSearchForm struct {
	Field  string       `form:"field" json:"field"`
	Vector types.Vector `form:"vector" json:"vector"`
	K      int          `form:"k" json:"k"`
	Filter string       `form:"filter" json:"filter"`
}

// vectorSearchResult is a single row of the vector search query.
type vectorSearchResult struct {
	Id       string  `db:"id"`
	Distance float64 `db:"__distance"`
}

// recordsVectorSearch returns the k records with the vector field closest to the
// provided vector, with the list rule and the optional filter applied.
//
// Each record has an extra "score" property (higher is closer):
// the cosine similarity (1 - cosine distance), the inner product or
// 1 / (1 + l2 distance), depending on the field metric.
func recordsVectorSearch(e *core.RequestEvent) error {
	collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || collection == nil {
		return e.NotFoundError("Missing collection context.", err)
	}

	err = checkCollectionRateLimit(e, collection, "list")
	if err != nil {
		return err
	}

	requestInfo, err := e.RequestInfo()
	if err != nil {
		return firstApiError(err, e.BadRequestError("", err))
	}

	isSuperuser := requestInfo.HasSuperuserAuth()

	if collection.ListRule == nil && !isSuperuser {
		return e.ForbiddenError("Only superusers can perform this action.", nil)
	}

	form := vectorSearchForm{}
	if err := e.BindBody(&form); err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while loading the submitted data.", err))
	}

	if form.K == 0 {
		form.K = vectorSearchDefaultK
	}

	field, _ := collection.Fields.GetByName(form.Field).(*core.VectorField)

	err = validation.ValidateStruct(&form,
		validation.Field(&form.Field, validation.Required, validation.By(func(value any) error {
			if field == nil || (field.Hidden && !isSuperuser) {
				return validation.NewError("validation_invalid_vector_field", "Missing or invalid vector field.")
			}
			return nil
		})),
		validation.Field(&form.Vector, validation.Required, validation.By(func(value any) error {
			if field != nil && len(form.Vector) != field.Dimensions {
				return validation.NewError(
					"validation_invalid_vector_dimensions",
					fmt.Sprintf("The vector must have %d dimensions.", field.Dimensions),
				)
			}
			return nil
		})),
		validation.Field(&form.K, validation.Min(1), validation.Max(vectorSearchMaxK)),
		validation.Field(&form.Filter, validation.Length(0, search.MaxFilterLength)),
	)
	if err != nil {
		return firstApiError(err, e.BadRequestError("An error occurred while validating the submitted data.", err))
	}

	// forbid users and guests to filter by special fields
	// (the same as the filter query parameter of the records list)
	if !isSuperuser {
		for _, field := range superuserOnlyRuleFields {
			if strings.Contains(form.Filter, field) {
				return e.ForbiddenError("Only superusers can filter by "+field, nil)
			}
		}
	}

	tableName := inflector.Columnify(collection.Name)
	column := "[[" + tableName + "." + inflector.Columnify(field.Name) + "]]"

	query := e.App.RecordQuery(collection)

	resolver := core.NewRecordFieldResolver(e.App, collection, requestInfo, true)

	if !isSuperuser && collection.ListRule != nil && *collection.ListRule != "" {
		expr, err := search.FilterData(*collection.ListRule).BuildExpr(resolver)
		if err != nil {
			return err
		}
		query.AndWhere(expr)
	}

	// hidden fields are searchable only by superusers
	resolver.SetAllowHiddenFields(isSuperuser)

	if form.Filter != "" {
		expr, err := search.FilterData(form.Filter).BuildExprWithLimit(resolver, search.DefaultFilterExprLimit)
		if err != nil {
			return firstApiError(err, e.BadRequestError("Invalid filter.", err))
		}
		query.AndWhere(expr)
	}

	if err := resolver.UpdateQuery(query); err != nil {
		return firstApiError(err, e.BadRequestError("", err))
	}

	// note: with DISTINCT (added by the joins) the order by
	// column must be in the select list, so the alias is used
	query.
		Select(
			"[["+tableName+".id]]",
			"("+column+" "+field.DistanceOperator()+" {:__vector}::vector) AS [[__distance]]",
		).
		AndWhere(dbx.NewExp(column + " IS NOT NULL")).
		AndBind(dbx.Params{"__vector": form.Vector.String()}).
		OrderBy("[[__distance]] ASC").
		Limit(int64(form.K))

	rows := []vectorSearchResult{}
	if err := query.All(&rows); err != nil {
		return firstApiError(err, e.BadRequestError("", err))
	}

	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.Id
	}

	found, err := e.App.FindRecordsByIds(collection, ids)
	if err != nil {
		return e.InternalServerError("Failed to load the records.", err)
	}

	byId := make(map[string]*core.Record, len(found))
	for _, record := range found {
		byId[record.Id] = record
	}

	records := make([]*core.Record, 0, len(rows))
	for _, row := range rows {
		record, ok := byId[row.Id]
		if !ok {
			continue // deleted meanwhile
		}

		record.WithCustomData(true)
		record.Set("score", vectorScore(field.MetricOrDefault(), row.Distance))

		records = append(records, record)
	}

	if err := EnrichRecords(e, records); err != nil {
		return firstApiError(err, e.InternalServerError("Failed to enrich records", err))
	}

	return execAfterSuccessTx(true, e.App, func() error {
		// the same format as the records list with skipTotal
		return e.JSON(http.StatusOK, &search.Result{
			Page:       1,
			PerPage:    form.K,
			TotalItems: -1,
			TotalPages: -1,
			Items:      records,
		})
	})
}

// vectorScore converts the pgvector distance of the provided metric
// to a score where higher is closer.
func vectorScore(metric string, distance float64) float64 {
	switch metric {
	case core.VectorMetricIP:
		// <#> returns the negative inner product
		return -distance
	case core.VectorMetricL2:
		return 1 / (1 + distance)
	default:
		return 1 - distance
	}
}
