package search

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ganigeorgiev/fexpr"
	"github.com/pocketbase/dbx"
	"github.com/spf13/cast"
)

// popgres: the full text search of the "search" fields (a generated tsvector column).

// DefaultTextSearchConfig is the text search configuration used when
// a "search" field doesn't specify one (no stemming and no stop words).
const DefaultTextSearchConfig = "simple"

// MaxTextSearchQueryLength is the max length of a text search query.
const MaxTextSearchQueryLength = 1000

var textSearchConfigRegex = regexp.MustCompile(`^[a-z_]{1,63}$`)

// IsValidTextSearchConfig reports whether the provided name could be
// a Postgres text search configuration name (eg. "english").
//
// The configs are rendered as SQL literals, so only lowercase
// letters and underscores are allowed.
func IsValidTextSearchConfig(name string) bool {
	return textSearchConfigRegex.MatchString(name)
}

// TextSearchConfigLiteral returns the regconfig SQL literal of the
// provided text search configuration (the default one if empty or invalid).
func TextSearchConfigLiteral(name string) string {
	if !IsValidTextSearchConfig(name) {
		name = DefaultTextSearchConfig
	}
	return "'" + name + "'::regconfig"
}

// TextSearchQueryExpr returns the tsquery SQL expression of the provided
// user query, parsed with the websearch syntax ("quoted phrases", OR, -word).
//
// The query identifier must be a SQL text expression (eg. a placeholder param).
func TextSearchQueryExpr(config string, queryIdentifier string) string {
	return "websearch_to_tsquery(" + TextSearchConfigLiteral(config) + ", " + queryIdentifier + ")"
}

// textSearchQueryLiteral returns the provided query as a text SQL expression
// for the places without params (eg. the sort expressions).
//
// The query is hex encoded so that it can't break out of the literal.
func textSearchQueryLiteral(query string) string {
	return "convert_from(decode('" + hex.EncodeToString([]byte(query)) + "', 'hex'), 'UTF8')"
}

// buildTextSearchExpr builds the full text search expression of a "search" field:
// "field ~ 'query'" matches the records with all the query words (websearch syntax)
// and "field !~ 'query'" the rest. As with LIKE, the empty query matches all records.
//
// Through a multiple relation the expression matches if at least one of the
// related records matches (there is no "match all" constraint).
func buildTextSearchExpr(left *ResolverResult, op fexpr.SignOp, right *ResolverResult) (dbx.Expression, error) {
	if left.Type != ValueTypeTextSearch || right.Type == ValueTypeTextSearch {
		return nil, errors.New("the search field must be the left operand, eg. content ~ 'query'")
	}

	var negate bool
	switch op {
	case fexpr.SignLike, fexpr.SignAnyLike:
	case fexpr.SignNlike, fexpr.SignAnyNlike:
		negate = true
	default:
		return nil, fmt.Errorf("the search fields support only the ~ and !~ operators, got %q", op)
	}

	if !isPlainParam(right) {
		return nil, errors.New("the search field must be compared with a text value, eg. content ~ 'query'")
	}

	var query string
	for _, v := range right.Params {
		query = strings.TrimSpace(cast.ToString(v))
	}

	if len(query) > MaxTextSearchQueryLength {
		return nil, fmt.Errorf("the text search query must be at most %d characters", MaxTextSearchQueryLength)
	}

	var expr dbx.Expression
	if query == "" {
		if negate {
			expr = dbx.NewExp("FALSE")
		} else {
			expr = dbx.NewExp("TRUE")
		}
	} else {
		match := left.Identifier + " @@ " + TextSearchQueryExpr(left.TextSearchConfig, right.Identifier)
		if negate {
			match = "NOT (" + match + ")"
		}
		expr = dbx.NewExp(match, mergeParams(left.Params, stringifyParams(right.Params)))
	}

	if left.AfterBuild != nil {
		expr = left.AfterBuild(expr)
	}

	if right.AfterBuild != nil {
		expr = right.AfterBuild(expr)
	}

	return expr, nil
}

// buildTextRankSort builds the relevance expression of the
// "@textRank(field, 'query')" sort field (higher is more relevant).
func buildTextRankSort(name string, fieldResolver FieldResolver) (string, error) {
	invalidErr := fmt.Errorf("invalid sort field %q, expected @textRank(searchField, 'query')", name)

	if !strings.HasSuffix(name, ")") {
		return "", invalidErr
	}

	args := splitSortFields(name[len(textRankSortKey) : len(name)-1])
	if len(args) != 2 {
		return "", invalidErr
	}

	result, err := fieldResolver.Resolve(strings.TrimSpace(args[0]))
	if err != nil || result.Type != ValueTypeTextSearch || len(result.Params) > 0 {
		return "", invalidErr
	}

	query, ok := unquoteSortArg(args[1])
	if !ok || len(query) > MaxTextSearchQueryLength {
		return "", invalidErr
	}

	return "ts_rank_cd(" + result.Identifier + ", " + TextSearchQueryExpr(result.TextSearchConfig, textSearchQueryLiteral(strings.TrimSpace(query))) + ")", nil
}

// unquoteSortArg returns the content of a single or double quoted sort argument.
func unquoteSortArg(arg string) (string, bool) {
	arg = strings.TrimSpace(arg)
	if len(arg) < 2 {
		return "", false
	}

	quote := arg[0]
	if (quote != '\'' && quote != '"') || arg[len(arg)-1] != quote {
		return "", false
	}

	inner := arg[1 : len(arg)-1]
	if strings.ContainsRune(inner, rune(quote)) {
		return "", false
	}

	return inner, true
}
