package search_test

import (
	"strings"
	"testing"

	"github.com/Robernetes/popgres/tools/search"
	"github.com/pocketbase/dbx"
)

// textSearchResolver resolves "fts" as the tsvector column of a search field
// and the other fields with a SimpleFieldResolver.
type textSearchResolver struct {
	*search.SimpleFieldResolver
}

func (r *textSearchResolver) Resolve(field string) (*search.ResolverResult, error) {
	if field == "fts" {
		return &search.ResolverResult{
			Identifier:       "[[fts]]",
			Type:             search.ValueTypeTextSearch,
			TextSearchConfig: "english",
		}, nil
	}
	return r.SimpleFieldResolver.Resolve(field)
}

func newTextSearchResolver() *textSearchResolver {
	return &textSearchResolver{search.NewSimpleFieldResolver("title")}
}

func TestTextSearchFilter(t *testing.T) {
	scenarios := []struct {
		filter      string
		expectError bool
		expectSQL   string
		expectQuery string
	}{
		{"fts ~ 'red shoes'", false, "[[fts]] @@ websearch_to_tsquery('english'::regconfig, {:", "red shoes"},
		{"fts ?~ 'red'", false, "[[fts]] @@ websearch_to_tsquery('english'::regconfig, {:", "red"},
		{"fts !~ 'red'", false, "NOT ([[fts]] @@ websearch_to_tsquery('english'::regconfig, {:", "red"},
		{"fts ~ 123", false, "[[fts]] @@ websearch_to_tsquery('english'::regconfig, {:", "123"},
		{"fts ~ ''", false, "TRUE", ""},
		{"fts !~ '  '", false, "FALSE", ""},
		{"fts ~ 'red' && title = 'a'", false, "[[fts]] @@ websearch_to_tsquery(", "red"},
		{"fts = 'red'", true, "", ""},
		{"fts > 'red'", true, "", ""},
		{"'red' ~ fts", true, "", ""},
		{"fts ~ fts", true, "", ""},
		{"fts ~ title", true, "", ""},
		{"fts ~ '" + strings.Repeat("a", search.MaxTextSearchQueryLength+1) + "'", true, "", ""},
	}

	for _, s := range scenarios {
		t.Run(s.filter, func(t *testing.T) {
			expr, err := search.FilterData(s.filter).BuildExpr(newTextSearchResolver())

			hasErr := err != nil
			if hasErr != s.expectError {
				t.Fatalf("Expected hasErr %v, got %v (%v)", s.expectError, hasErr, err)
			}
			if hasErr {
				return
			}

			params := dbx.Params{}
			sql := expr.Build(nil, params)
			if !strings.Contains(sql, s.expectSQL) {
				t.Fatalf("Expected %q in %q", s.expectSQL, sql)
			}

			if s.expectQuery == "" {
				return
			}

			var found bool
			for _, v := range params {
				if v == s.expectQuery {
					found = true
				}
			}
			if !found {
				t.Fatalf("Expected the query param %q, got %v", s.expectQuery, params)
			}
		})
	}
}

func TestTextRankSort(t *testing.T) {
	scenarios := []struct {
		sort        string
		expectError bool
		expectSQL   string
	}{
		{
			"@textRank(fts, 'red shoes')",
			false,
			"ts_rank_cd([[fts]], websearch_to_tsquery('english'::regconfig, convert_from(decode('7265642073686f6573', 'hex'), 'UTF8'))) ASC",
		},
		{
			// the quote is hex encoded so it can't break the literal
			`@textRank(fts, "it's, ok")`,
			false,
			"convert_from(decode('697427732c206f6b', 'hex'), 'UTF8')",
		},
		{"@textRank(fts)", true, ""},
		{"@textRank(fts, red)", true, ""},
		{"@textRank(fts, 'red', 'blue')", true, ""},
		{"@textRank(title, 'red')", true, ""},
		{"@textRank(unknown, 'red')", true, ""},
		{"@textRank(fts, 'a'b')", true, ""},
		{"@textRank(fts, 'red'", true, ""},
	}

	for _, s := range scenarios {
		t.Run(s.sort, func(t *testing.T) {
			sortField := search.SortField{Name: s.sort, Direction: search.SortAsc}

			expr, err := sortField.BuildExpr(newTextSearchResolver())

			hasErr := err != nil
			if hasErr != s.expectError {
				t.Fatalf("Expected hasErr %v, got %v (%v)", s.expectError, hasErr, err)
			}
			if hasErr {
				return
			}

			if !strings.Contains(expr, s.expectSQL) {
				t.Fatalf("Expected %q in %q", s.expectSQL, expr)
			}
		})
	}
}

func TestParseSortFromStringQuotes(t *testing.T) {
	fields := search.ParseSortFromString("-@textRank(fts, 'a, b (c'),title")

	if len(fields) != 2 {
		t.Fatalf("Expected 2 sort fields, got %v", fields)
	}

	if fields[0].Name != "@textRank(fts, 'a, b (c')" || fields[0].Direction != search.SortDesc {
		t.Fatalf("Unexpected first sort field %v", fields[0])
	}

	if fields[1].Name != "title" || fields[1].Direction != search.SortAsc {
		t.Fatalf("Unexpected second sort field %v", fields[1])
	}
}
