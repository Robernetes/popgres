package dbutils_test

import (
	"testing"

	"github.com/Robernetes/popgres/tools/dbutils"
)

func TestJSONEach(t *testing.T) {
	result := dbutils.JSONEach("a.b")

	expected := "jsonb_array_elements_text(popgres_json_array([[a.b]]))"

	if result != expected {
		t.Fatalf("Expected\n%v\ngot\n%v", expected, result)
	}
}

func TestJSONArrayLength(t *testing.T) {
	result := dbutils.JSONArrayLength("a.b")

	expected := "popgres_json_array_length([[a.b]])"

	if result != expected {
		t.Fatalf("Expected\n%v\ngot\n%v", expected, result)
	}
}

func TestJSONExtract(t *testing.T) {
	scenarios := []struct {
		name     string
		column   string
		path     string
		expected string
	}{
		{
			"empty path",
			"a.b",
			"",
			"NULLIF(popgres_jsonb([[a.b]]), 'null'::jsonb)",
		},
		{
			"starting with array index",
			"a.b",
			"[1].a[2]",
			`NULLIF(jsonb_path_query_first(popgres_jsonb([[a.b]]), 'strict $[1]."a"[2]', '{}', true), 'null'::jsonb)`,
		},
		{
			"starting with key",
			"a.b",
			"a.b[2].c",
			`NULLIF(jsonb_path_query_first(popgres_jsonb([[a.b]]), 'strict $."a"."b"[2]."c"', '{}', true), 'null'::jsonb)`,
		},
		{
			"key with quotes",
			"a.b",
			`a"'b`,
			`NULLIF(jsonb_path_query_first(popgres_jsonb([[a.b]]), 'strict $."a\"''b"', '{}', true), 'null'::jsonb)`,
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := dbutils.JSONExtract(s.column, s.path)

			if result != s.expected {
				t.Fatalf("Expected\n%v\ngot\n%v", s.expected, result)
			}
		})
	}
}
