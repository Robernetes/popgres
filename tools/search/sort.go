package search

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	randomSortKey string = "@random"
	rowidSortKey  string = "@rowid"

	// popgres: @vectorDistance(field, [1,2,3]) sorts by the pgvector
	// distance (the optional third argument is the metric: cosine, l2 or ip)
	vectorDistanceSortKey string = "@vectorDistance("
)

// sort field directions
const (
	SortAsc  string = "ASC"
	SortDesc string = "DESC"
)

// SortField defines a single search sort field.
type SortField struct {
	Name      string `json:"name"`
	Direction string `json:"direction"`
}

// BuildExpr resolves the sort field into a valid db sort expression.
func (s *SortField) BuildExpr(fieldResolver FieldResolver) (string, error) {
	// special case for random sort
	if s.Name == randomSortKey {
		return "RANDOM()", nil
	}

	// popgres: pgvector distance
	if strings.HasPrefix(s.Name, vectorDistanceSortKey) {
		expr, err := buildVectorDistanceSort(s.Name, fieldResolver)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s %s", expr, s.Direction), nil
	}

	// the SQLite rowid column doesn't exist in Postgres
	if s.Name == rowidSortKey {
		return "", fmt.Errorf("the %q sort is not supported, use a regular field like created or id instead", rowidSortKey)
	}

	result, err := fieldResolver.Resolve(s.Name)

	// invalidate empty fields and non-column identifiers
	if err != nil || len(result.Params) > 0 || result.Identifier == "" || strings.EqualFold(result.Identifier, "null") {
		return "", fmt.Errorf("invalid sort field %q", s.Name)
	}

	return fmt.Sprintf("%s %s", result.Identifier, s.Direction), nil
}

// ParseSortFromString parses the provided string expression
// into a slice of SortFields.
//
// Example:
//
//	fields := search.ParseSortFromString("-name,+created")
func ParseSortFromString(str string) (fields []SortField) {
	data := splitSortFields(str)

	for _, field := range data {
		// trim whitespaces
		field = strings.TrimSpace(field)
		if strings.HasPrefix(field, "-") {
			fields = append(fields, SortField{strings.TrimPrefix(field, "-"), SortDesc})
		} else {
			fields = append(fields, SortField{strings.TrimPrefix(field, "+"), SortAsc})
		}
	}

	return
}

// splitSortFields splits the sort expression by the commas that are not
// inside parenthesis or brackets (eg. the @vectorDistance arguments).
func splitSortFields(str string) []string {
	var parts []string
	var depth int
	var start int

	for i, ch := range str {
		switch ch {
		case '(', '[':
			depth++
		case ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, str[start:i])
				start = i + 1
			}
		}
	}

	return append(parts, str[start:])
}

// buildVectorDistanceSort builds the pgvector distance expression of
// the "@vectorDistance(field, [1,2,3], metric)" sort field.
func buildVectorDistanceSort(name string, fieldResolver FieldResolver) (string, error) {
	invalidErr := fmt.Errorf("invalid sort field %q, expected @vectorDistance(field, [numbers...], [metric])", name)

	if !strings.HasSuffix(name, ")") {
		return "", invalidErr
	}

	args := splitSortFields(name[len(vectorDistanceSortKey) : len(name)-1])
	if len(args) < 2 || len(args) > 3 {
		return "", invalidErr
	}

	result, err := fieldResolver.Resolve(strings.TrimSpace(args[0]))
	if err != nil || len(result.Params) > 0 || result.Identifier == "" || strings.EqualFold(result.Identifier, "null") {
		return "", invalidErr
	}

	// the vector is validated and rendered as a literal because the sort
	// expressions don't have params (each item is a parsed float number)
	rawVector := strings.TrimSpace(args[1])
	if !strings.HasPrefix(rawVector, "[") || !strings.HasSuffix(rawVector, "]") {
		return "", invalidErr
	}

	items := strings.Split(rawVector[1:len(rawVector)-1], ",")
	if len(items) == 0 || len(items) > 16000 {
		return "", invalidErr
	}

	normalized := make([]string, len(items))
	for i, item := range items {
		n, err := strconv.ParseFloat(strings.TrimSpace(item), 32)
		if err != nil {
			return "", invalidErr
		}
		normalized[i] = strconv.FormatFloat(n, 'g', -1, 32)
	}

	operator := "<=>"
	if len(args) == 3 {
		switch strings.Trim(strings.TrimSpace(args[2]), `'"`) {
		case "cosine":
			operator = "<=>"
		case "l2":
			operator = "<->"
		case "ip":
			operator = "<#>"
		default:
			return "", invalidErr
		}
	}

	return "(" + result.Identifier + " " + operator + " '[" + strings.Join(normalized, ",") + "]'::vector)", nil
}
