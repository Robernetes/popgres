package validators_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Robernetes/popgres/core/validators"
	"github.com/Robernetes/popgres/tests"
	"github.com/jackc/pgx/v5/pgconn"
	validation "github.com/pocketbase/ozzo-validation/v4"
)

func TestUniqueId(t *testing.T) {
	t.Parallel()

	app, _ := tests.NewTestApp()
	defer app.Cleanup()

	scenarios := []struct {
		id          string
		tableName   string
		expectError bool
	}{
		{"", "", false},
		{"test", "", true},
		{"wsmn24bux7wo113", "_collections", true},
		{"test_unique_id", "unknown_table", true},
		{"test_unique_id", "_collections", false},
	}

	for i, s := range scenarios {
		t.Run(fmt.Sprintf("%d_%s_%s", i, s.id, s.tableName), func(t *testing.T) {
			err := validators.UniqueId(app.DB(), s.tableName)(s.id)

			hasErr := err != nil
			if hasErr != s.expectError {
				t.Fatalf("Expected hasErr to be %v, got %v (%v)", s.expectError, hasErr, err)
			}
		})
	}
}

func TestNormalizeUniqueIndexError(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name         string
		err          error
		table        string
		names        []string
		expectedKeys []string
	}{
		{
			"nil error (no changes)",
			nil,
			"test",
			[]string{"a", "b"},
			nil,
		},
		{
			"non-unique index error (no changes)",
			errors.New("abc"),
			"test",
			[]string{"a", "b"},
			nil,
		},
		{
			"validation error (no changes)",
			validation.Errors{"c": errors.New("abc")},
			"test",
			[]string{"a", "b"},
			[]string{"c"},
		},
		{
			"non-unique violation postgres error (no changes)",
			&pgconn.PgError{Code: "23503", TableName: "test", Detail: "Key (a)=(1) is not present"},
			"test",
			[]string{"a", "b"},
			nil,
		},
		{
			"unique violation but mismatched table name",
			&pgconn.PgError{Code: "23505", TableName: "test", Detail: "Key (a, b)=(1, 2) already exists."},
			"example",
			[]string{"a", "b"},
			nil,
		},
		{
			"unique violation but mismatched fields",
			&pgconn.PgError{Code: "23505", TableName: "test", Detail: "Key (a, b)=(1, 2) already exists."},
			"test",
			[]string{"c", "d"},
			nil,
		},
		{
			"unique violation with matching table name and fields",
			fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "23505", TableName: "test", Detail: "Key (a, b)=(1, 2) already exists."}),
			"test",
			[]string{"a", "b", "c"},
			[]string{"a", "b"},
		},
		{
			"unique violation with a field name prefix of another field",
			&pgconn.PgError{Code: "23505", TableName: "test", Detail: "Key (a_2, c)=(1, 2) already exists."},
			"test",
			[]string{"a", "a_2", "c"},
			[]string{"a_2", "c"},
		},
		{
			"unique violation of an expression index with quoted mixed case column",
			&pgconn.PgError{Code: "23505", TableName: "Test", Detail: `Key (lower("tokenKey"))=(abc) already exists.`},
			"test",
			[]string{"tokenKey", "email"},
			[]string{"tokenKey"},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			result := validators.NormalizeUniqueIndexError(s.err, s.table, s.names)

			if len(s.expectedKeys) == 0 {
				if result != s.err {
					t.Fatalf("Expected no error change, got %v", result)
				}
				return
			}

			tests.TestValidationErrors(t, result, s.expectedKeys)
		})
	}
}
