package core_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/Robernetes/popgres/core"
	"github.com/Robernetes/popgres/tests"
	"github.com/Robernetes/popgres/tools/types"
)

func TestVectorFieldBaseMethods(t *testing.T) {
	testFieldBaseMethods(t, core.FieldTypeVector)
}

func TestVectorFieldColumnType(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &core.VectorField{Dimensions: 3}

	if v := f.ColumnType(app); v != "vector(3)" {
		t.Fatalf("Expected vector(3), got %q", v)
	}
}

func TestVectorFieldPrepareValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	f := &core.VectorField{Dimensions: 3}
	record := core.NewRecord(core.NewBaseCollection("test"))

	scenarios := []struct {
		raw         any
		expected    string
		expectError bool
	}{
		{nil, `null`, false},
		{"", `null`, false},
		{"[]", `null`, false},
		{[]byte{}, `null`, false},
		{types.Vector{1, 2.5, 3}, `[1,2.5,3]`, false},
		{[]float64{1, 2.5, 3}, `[1,2.5,3]`, false},
		{[]any{1.0, 2.5, 3.0}, `[1,2.5,3]`, false},
		{"[1, 2.5, 3]", `[1,2.5,3]`, false},
		{[]byte("[1,2.5,3]"), `[1,2.5,3]`, false},
		{"1,2,3", ``, true},
		{"[1, \"a\"]", ``, true},
		{123, ``, true},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%#v", i, s.raw), func(t *testing.T) {
			v, err := f.PrepareValue(record, s.raw)

			hasErr := err != nil
			if hasErr != s.expectError {
				t.Fatalf("Expected hasErr %v, got %v (%v)", s.expectError, hasErr, err)
			}
			if hasErr {
				return
			}

			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}

			if string(raw) != s.expected {
				t.Fatalf("Expected %s, got %s", s.expected, raw)
			}
		})
	}
}

func TestVectorFieldValidateValue(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("test_collection")

	scenarios := []struct {
		name        string
		field       *core.VectorField
		record      func() *core.Record
		expectError bool
	}{
		{
			"invalid raw value",
			&core.VectorField{Name: "test", Dimensions: 3},
			func() *core.Record {
				record := core.NewRecord(collection)
				record.SetRaw("test", 123)
				return record
			},
			true,
		},
		{
			"zero value (non-required)",
			&core.VectorField{Name: "test", Dimensions: 3},
			func() *core.Record {
				record := core.NewRecord(collection)
				record.SetRaw("test", types.Vector{})
				return record
			},
			false,
		},
		{
			"zero value (required)",
			&core.VectorField{Name: "test", Dimensions: 3, Required: true},
			func() *core.Record {
				record := core.NewRecord(collection)
				record.SetRaw("test", types.Vector{})
				return record
			},
			true,
		},
		{
			"different dimensions",
			&core.VectorField{Name: "test", Dimensions: 3},
			func() *core.Record {
				record := core.NewRecord(collection)
				record.SetRaw("test", types.Vector{1, 2})
				return record
			},
			true,
		},
		{
			"valid vector",
			&core.VectorField{Name: "test", Dimensions: 3, Required: true},
			func() *core.Record {
				record := core.NewRecord(collection)
				record.SetRaw("test", types.Vector{1, 2, 3})
				return record
			},
			false,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			err := s.field.ValidateValue(context.Background(), app, s.record())

			hasErr := err != nil
			if hasErr != s.expectError {
				t.Fatalf("Expected hasErr %v, got %v (%v)", s.expectError, hasErr, err)
			}
		})
	}
}

func TestVectorFieldValidateSettings(t *testing.T) {
	testDefaultFieldIdValidation(t, core.FieldTypeVector)
	testDefaultFieldNameValidation(t, core.FieldTypeVector)

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		name         string
		field        func() *core.VectorField
		expectErrors []string
	}{
		{
			"zero minimal",
			func() *core.VectorField {
				return &core.VectorField{Id: "test", Name: "test"}
			},
			[]string{"dimensions"},
		},
		{
			"invalid metric",
			func() *core.VectorField {
				return &core.VectorField{Id: "test", Name: "test", Dimensions: 3, Metric: "abc"}
			},
			[]string{"metric"},
		},
		{
			"too many dimensions",
			func() *core.VectorField {
				return &core.VectorField{Id: "test", Name: "test", Dimensions: core.VectorMaxDimensions + 1}
			},
			[]string{"dimensions"},
		},
		{
			"too many dimensions for an index",
			func() *core.VectorField {
				return &core.VectorField{Id: "test", Name: "test", Dimensions: core.VectorMaxIndexDimensions + 1, Index: true}
			},
			[]string{"dimensions"},
		},
		{
			"valid",
			func() *core.VectorField {
				return &core.VectorField{Id: "test", Name: "test", Dimensions: 1536, Metric: core.VectorMetricL2, Index: true}
			},
			[]string{},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			collection := core.NewBaseCollection("test_collection")
			field := s.field()
			collection.Fields.Add(field)

			errs := field.ValidateSettings(context.Background(), app, collection)

			tests.TestValidationErrors(t, errs, s.expectErrors)
		})
	}
}

// TestVectorFieldRecordsAndIndex checks the vector column, the HNSW index and the
// dimensions change check with a real collection.
func TestVectorFieldRecordsAndIndex(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	collection := core.NewBaseCollection("vectors")
	collection.Fields.Add(&core.VectorField{Name: "embedding", Dimensions: 3, Index: true})
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	field := collection.Fields.GetByName("embedding").(*core.VectorField)

	var indexDef string
	err := app.DB().NewQuery("SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_vec_" + collection.Id + "_" + field.Id + "'").Row(&indexDef)
	if err != nil {
		t.Fatalf("Expected the HNSW index: %v", err)
	}
	if !strings.Contains(indexDef, "hnsw") || !strings.Contains(indexDef, "vector_cosine_ops") {
		t.Fatalf("Unexpected index definition %q", indexDef)
	}

	record := core.NewRecord(collection)
	record.Set("embedding", []float64{0.1, 0.2, 0.3})
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}

	empty := core.NewRecord(collection)
	if err := app.Save(empty); err != nil {
		t.Fatal(err)
	}

	found, err := app.FindRecordById(collection, record.Id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(found.Get("embedding"))
	if string(raw) != "[0.1,0.2,0.3]" {
		t.Fatalf("Expected [0.1,0.2,0.3], got %s", raw)
	}

	found, err = app.FindRecordById(collection, empty.Id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(found.Get("embedding"))
	if string(raw) != "null" {
		t.Fatalf("Expected null, got %s", raw)
	}

	// the dimensions can't be changed
	field.Dimensions = 4
	err = app.Save(collection)
	if err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("Expected a dimensions change error, got %v", err)
	}

	// disable the index
	field.Dimensions = 3
	field.Index = false
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	var total int
	err = app.DB().NewQuery("SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_vec_" + collection.Id + "_" + field.Id + "'").Row(&total)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatal("Expected the HNSW index to be dropped")
	}
}
