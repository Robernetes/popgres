package search_test

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/Robernetes/popgres/tools/search"
)

func TestSortFieldBuildExpr(t *testing.T) {
	resolver := search.NewSimpleFieldResolver("test1", "test2", "test3", "test4.sub")

	scenarios := []struct {
		sortField        search.SortField
		expectError      bool
		expectExpression string
	}{
		// empty
		{search.SortField{"", search.SortDesc}, true, ""},
		// unknown field
		{search.SortField{"unknown", search.SortAsc}, true, ""},
		// placeholder field
		{search.SortField{"'test'", search.SortAsc}, true, ""},
		// null field
		{search.SortField{"null", search.SortAsc}, true, ""},
		// allowed field - asc
		{search.SortField{"test1", search.SortAsc}, false, "[[test1]] ASC"},
		// allowed field - desc
		{search.SortField{"test1", search.SortDesc}, false, "[[test1]] DESC"},
		// special @random field (ignore direction)
		{search.SortField{"@random", search.SortDesc}, false, "RANDOM()"},
		// special _rowid_ field
		// popgres: there is no rowid in Postgres
		{search.SortField{"@rowid", search.SortDesc}, true, ""},
		// popgres: pgvector distance
		{search.SortField{"@vectorDistance(test1, [1, 2.5,-3])", search.SortAsc}, false, "([[test1]] <=> '[1,2.5,-3]'::vector) ASC"},
		{search.SortField{"@vectorDistance(test1,[1,2],l2)", search.SortDesc}, false, "([[test1]] <-> '[1,2]'::vector) DESC"},
		{search.SortField{"@vectorDistance(test1,[1,2],'ip')", search.SortAsc}, false, "([[test1]] <#> '[1,2]'::vector) ASC"},
		{search.SortField{"@vectorDistance(test1,[1,2],abc)", search.SortAsc}, true, ""},
		{search.SortField{"@vectorDistance(unknown,[1,2])", search.SortAsc}, true, ""},
		{search.SortField{"@vectorDistance(test1,[1,'a'])", search.SortAsc}, true, ""},
		{search.SortField{"@vectorDistance(test1,[1,2]); DROP TABLE x", search.SortAsc}, true, ""},
		{search.SortField{"@vectorDistance(test1)", search.SortAsc}, true, ""},
	}

	for _, s := range scenarios {
		t.Run(fmt.Sprintf("%s_%s", s.sortField.Name, s.sortField.Name), func(t *testing.T) {
			result, err := s.sortField.BuildExpr(resolver)

			hasErr := err != nil
			if hasErr != s.expectError {
				t.Fatalf("Expected hasErr %v, got %v (%v)", s.expectError, hasErr, err)
			}

			if result != s.expectExpression {
				t.Fatalf("Expected expression %v, got %v", s.expectExpression, result)
			}
		})
	}
}

func TestParseSortFromString(t *testing.T) {
	scenarios := []struct {
		value    string
		expected string
	}{
		{"", `[{"name":"","direction":"ASC"}]`},
		{"test", `[{"name":"test","direction":"ASC"}]`},
		{"+test", `[{"name":"test","direction":"ASC"}]`},
		{"-test", `[{"name":"test","direction":"DESC"}]`},
		{"test1,-test2,+test3", `[{"name":"test1","direction":"ASC"},{"name":"test2","direction":"DESC"},{"name":"test3","direction":"ASC"}]`},
		{"@random,-test", `[{"name":"@random","direction":"ASC"},{"name":"test","direction":"DESC"}]`},
		{"-@rowid,-test", `[{"name":"@rowid","direction":"DESC"},{"name":"test","direction":"DESC"}]`},
		{"-@vectorDistance(a,[1,2]),b", `[{"name":"@vectorDistance(a,[1,2])","direction":"DESC"},{"name":"b","direction":"ASC"}]`},
	}

	for _, s := range scenarios {
		t.Run(s.value, func(t *testing.T) {
			result := search.ParseSortFromString(s.value)
			encoded, _ := json.Marshal(result, json.Deterministic(true))
			encodedStr := string(encoded)

			if encodedStr != s.expected {
				t.Fatalf("Expected expression %s, got %s", s.expected, encodedStr)
			}
		})
	}
}
