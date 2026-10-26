package core

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/Robernetes/popgres/core/validators"
	"github.com/Robernetes/popgres/tools/types"
	validation "github.com/pocketbase/ozzo-validation/v4"
)

// popgres: the pgvector "vector" field.

func init() {
	Fields[FieldTypeVector] = func() Field {
		return &VectorField{}
	}
}

const FieldTypeVector = "vector"

// The vector field distance metrics.
const (
	VectorMetricCosine = "cosine"
	VectorMetricL2     = "l2"
	VectorMetricIP     = "ip"
)

// The pgvector limits.
const (
	VectorMaxDimensions      = 16000
	VectorMaxIndexDimensions = 2000
)

var (
	_ Field = (*VectorField)(nil)
)

// VectorField defines "vector" type field for storing pgvector embeddings
// (a fixed size list of float32 numbers).
//
// The record field value is always [types.Vector]. It could be set as
// [types.Vector], a slice of numbers or a serialized json array.
// The empty vector is stored as NULL and serialized as JSON null.
//
// Examples of updating a record's VectorField value programmatically:
//
//	record.Set("embedding", types.Vector{0.1, 0.2, 0.3})
//	record.Set("embedding", []float64{0.1, 0.2, 0.3})
//	record.Set("embedding", "[0.1, 0.2, 0.3]")
//
// The field requires the pgvector extension on the Postgres server
// (it is created with the first vector field if the database role can create it).
type VectorField struct {
	// Name (required) is the unique name of the field.
	Name string `form:"name" json:"name"`

	// Id is the unique stable field identifier.
	//
	// It is automatically generated from the name when adding to a collection FieldsList.
	Id string `form:"id" json:"id"`

	// System prevents the renaming and removal of the field.
	System bool `form:"system" json:"system"`

	// Hidden hides the field from the API response.
	Hidden bool `form:"hidden" json:"hidden"`

	// Presentable hints the Dashboard UI to use the underlying
	// field record value in the relation preview label.
	Presentable bool `form:"presentable" json:"presentable"`

	// ---

	// Help is an extra text explaining what the field is about.
	// It is usually shown in Dashboard UI under the field input.
	Help string `form:"help" json:"help"`

	// Dimensions (required) is the number of the vector items.
	//
	// It can't be changed after the field is created.
	Dimensions int `form:"dimensions" json:"dimensions"`

	// Metric is the distance metric used by the vector search
	// and the field index: "cosine" (default), "l2" or "ip" (inner product).
	Metric string `form:"metric" json:"metric"`

	// Index creates an HNSW index for the field metric
	// (supported for up to 2000 dimensions).
	Index bool `form:"index" json:"index"`

	// Required will require the field value to be non-empty.
	Required bool `form:"required" json:"required"`
}

// Type implements [Field.Type] interface method.
func (f *VectorField) Type() string {
	return FieldTypeVector
}

// GetId implements [Field.GetId] interface method.
func (f *VectorField) GetId() string {
	return f.Id
}

// SetId implements [Field.SetId] interface method.
func (f *VectorField) SetId(id string) {
	f.Id = id
}

// GetName implements [Field.GetName] interface method.
func (f *VectorField) GetName() string {
	return f.Name
}

// SetName implements [Field.SetName] interface method.
func (f *VectorField) SetName(name string) {
	f.Name = name
}

// GetSystem implements [Field.GetSystem] interface method.
func (f *VectorField) GetSystem() bool {
	return f.System
}

// SetSystem implements [Field.SetSystem] interface method.
func (f *VectorField) SetSystem(system bool) {
	f.System = system
}

// GetHidden implements [Field.GetHidden] interface method.
func (f *VectorField) GetHidden() bool {
	return f.Hidden
}

// SetHidden implements [Field.SetHidden] interface method.
func (f *VectorField) SetHidden(hidden bool) {
	f.Hidden = hidden
}

// ColumnType implements [Field.ColumnType] interface method.
func (f *VectorField) ColumnType(app App) string {
	return "vector(" + strconv.Itoa(f.Dimensions) + ")"
}

// MetricOrDefault returns the field distance metric (cosine if not set).
func (f *VectorField) MetricOrDefault() string {
	if f.Metric == "" {
		return VectorMetricCosine
	}
	return f.Metric
}

// DistanceOperator returns the pgvector distance operator of the field metric.
func (f *VectorField) DistanceOperator() string {
	switch f.MetricOrDefault() {
	case VectorMetricL2:
		return "<->"
	case VectorMetricIP:
		return "<#>"
	default:
		return "<=>"
	}
}

// PrepareValue implements [Field.PrepareValue] interface method.
func (f *VectorField) PrepareValue(record *Record, raw any) (any, error) {
	v := types.Vector{}
	err := v.Scan(raw)
	return v, err
}

// ValidateValue implements [Field.ValidateValue] interface method.
func (f *VectorField) ValidateValue(ctx context.Context, app App, record *Record) error {
	val, ok := record.GetRaw(f.Name).(types.Vector)
	if !ok {
		return validators.ErrUnsupportedValueType
	}

	if len(val) == 0 {
		if f.Required {
			return validation.ErrRequired
		}
		return nil
	}

	if len(val) != f.Dimensions {
		return validation.NewError(
			"validation_invalid_vector_dimensions",
			fmt.Sprintf("The vector must have %d dimensions, got %d.", f.Dimensions, len(val)),
		).SetParams(map[string]any{"expected": f.Dimensions, "got": len(val)})
	}

	return nil
}

// ValidateSettings implements [Field.ValidateSettings] interface method.
func (f *VectorField) ValidateSettings(ctx context.Context, app App, collection *Collection) error {
	maxDimensions := VectorMaxDimensions
	if f.Index {
		maxDimensions = VectorMaxIndexDimensions
	}

	return validation.ValidateStruct(f,
		validation.Field(&f.Id, validation.By(DefaultFieldIdValidationRule)),
		validation.Field(&f.Name, validation.By(DefaultFieldNameValidationRule)),
		validation.Field(&f.Help, validation.By(DefaultFieldHelpValidationRule)),
		validation.Field(
			&f.Dimensions,
			validation.Required,
			validation.Min(1),
			validation.Max(maxDimensions),
			validation.By(f.checkDimensionsChange(app, collection)),
			validation.By(f.checkExtension(app)),
		),
		validation.Field(&f.Metric, validation.In(VectorMetricCosine, VectorMetricL2, VectorMetricIP)),
	)
}

// checkDimensionsChange forbids changing the dimensions of an existing field
// (the existing column values can't be converted).
func (f *VectorField) checkDimensionsChange(app App, collection *Collection) validation.RuleFunc {
	return func(value any) error {
		if collection.IsNew() || f.Id == "" {
			return nil
		}

		old, err := app.FindCollectionByNameOrId(collection.Id)
		if err != nil {
			return nil // new collection
		}

		oldField, ok := old.Fields.GetById(f.Id).(*VectorField)
		if !ok || oldField.Dimensions == f.Dimensions {
			return nil
		}

		return validation.NewError(
			"validation_vector_dimensions_change",
			"The vector dimensions can't be changed. Create a new field instead.",
		)
	}
}

// checkExtension checks that the pgvector extension is installed
// or that the database role can install it.
func (f *VectorField) checkExtension(app App) validation.RuleFunc {
	return func(value any) error {
		status, err := VectorExtensionStatus(app)
		if err != nil {
			return validation.NewError("validation_vector_extension", "Failed to check the pgvector extension: "+err.Error())
		}

		switch status {
		case VectorExtensionInstalled, VectorExtensionInstallable:
			return nil
		case VectorExtensionUnavailable:
			return validation.NewError(
				"validation_vector_extension_unavailable",
				"The pgvector extension is not available on the Postgres server (the embedded dev Postgres doesn't include it, use a server with pgvector, eg. the pgvector/pgvector Docker image).",
			)
		default:
			return validation.NewError(
				"validation_vector_extension_permission",
				"The pgvector extension is not installed and the database role can't install it. Ask a database administrator to execute: CREATE EXTENSION vector;",
			)
		}
	}
}

// The pgvector extension statuses (see [VectorExtensionStatus]).
const (
	VectorExtensionInstalled    = "installed"
	VectorExtensionInstallable  = "installable"
	VectorExtensionNoPermission = "noPermission"
	VectorExtensionUnavailable  = "unavailable"
)

// VectorExtensionStatus reports whether the pgvector extension is installed,
// could be installed by the current database role (pgvector is not a trusted
// extension, so only a superuser can create it), is not permitted or is not
// available on the server at all.
func VectorExtensionStatus(app App) (string, error) {
	var installed, available, superuser bool

	err := app.DB().NewQuery(`
		SELECT
			EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector'),
			EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector'),
			(SELECT rolsuper FROM pg_roles WHERE rolname = current_user)
	`).Row(&installed, &available, &superuser)
	if err != nil {
		return "", err
	}

	switch {
	case installed:
		return VectorExtensionInstalled, nil
	case !available:
		return VectorExtensionUnavailable, nil
	case superuser:
		return VectorExtensionInstallable, nil
	default:
		return VectorExtensionNoPermission, nil
	}
}

// ensureVectorExtension creates the pgvector extension if the collection has a vector field.
func ensureVectorExtension(txApp App, collection *Collection) error {
	hasVector := slices.ContainsFunc(collection.Fields, func(f Field) bool {
		return f.Type() == FieldTypeVector
	})
	if !hasVector {
		return nil
	}

	status, err := VectorExtensionStatus(txApp)
	if err != nil {
		return err
	}
	if status == VectorExtensionInstalled {
		return nil
	}

	_, err = txApp.DB().NewQuery("CREATE EXTENSION IF NOT EXISTS vector").Execute()
	if err != nil {
		return fmt.Errorf("failed to create the pgvector extension (ask a database administrator to execute \"CREATE EXTENSION vector;\"): %w", err)
	}

	return nil
}

// vectorIndexName returns the name of the HNSW index of the provided field.
func vectorIndexName(collection *Collection, field *VectorField) string {
	return "idx_vec_" + collection.Id + "_" + field.Id
}

// vectorOperatorClass returns the pgvector index operator class of the field metric.
func vectorOperatorClass(field *VectorField) string {
	switch field.MetricOrDefault() {
	case VectorMetricL2:
		return "vector_l2_ops"
	case VectorMetricIP:
		return "vector_ip_ops"
	default:
		return "vector_cosine_ops"
	}
}

// dropVectorIndexes drops the HNSW indexes of the collection vector fields.
func dropVectorIndexes(txApp App, collection *Collection) error {
	for _, f := range collection.Fields {
		field, ok := f.(*VectorField)
		if !ok {
			continue
		}

		_, err := txApp.DB().NewQuery("DROP INDEX IF EXISTS [[" + vectorIndexName(collection, field) + "]]").Execute()
		if err != nil {
			return err
		}
	}

	return nil
}

// createVectorIndexes creates the HNSW indexes of the collection vector fields with enabled index.
func createVectorIndexes(txApp App, collection *Collection) error {
	for _, f := range collection.Fields {
		field, ok := f.(*VectorField)
		if !ok || !field.Index {
			continue
		}

		_, err := txApp.DB().NewQuery(fmt.Sprintf(
			"CREATE INDEX IF NOT EXISTS [[%s]] ON {{%s}} USING hnsw ([[%s]] %s)",
			vectorIndexName(collection, field),
			collection.Name,
			field.Name,
			vectorOperatorClass(field),
		)).Execute()
		if err != nil {
			return fmt.Errorf("failed to create the %q vector index: %w", field.Name, err)
		}
	}

	return nil
}
