package search

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
)

// Postgres specific operand normalizations.
//
// SQLite compares values with dynamic types while Postgres requires both
// operands of a comparison to have compatible types. To keep the filter
// semantic the resolved operands are normalized before building an expression:
//
//   - the params are sent as text and Postgres infers their type from the other
//     operand (eg. a number column), except when both operands are plain params
//     where the number and bool params are explicitly casted;
//   - when one of the operands is a jsonb value (ResolverResult.JSON) the other
//     operand is converted to jsonb based on its Go value type, so that eg.
//     json.a = 1 compares numbers and json.a = 'x' compares strings.

var plainParamRegex = regexp.MustCompile(`^\{:(\w+)\}$`)

// normalizeOperands returns normalized copies of the provided resolved operands.
//
// Set convertJSON to false for the operators that compare the text
// representation of the operands (eg. LIKE).
//
// The original results (and their params maps) are not modified because they
// could be reused for building the multi-match expressions.
func normalizeOperands(left, right *ResolverResult, convertJSON bool) (*ResolverResult, *ResolverResult) {
	l := *left
	r := *right

	switch {
	case !convertJSON:
		// nothing to convert
	case l.JSON && !r.JSON:
		r.Identifier = jsonbOperand(&r)
		r.JSON = true
	case r.JSON && !l.JSON:
		l.Identifier = jsonbOperand(&l)
		l.JSON = true
	case !l.JSON && !r.JSON && isPlainParam(&l) && isPlainParam(&r):
		// cast only params of the same kind (eg. 10 > 9); the mixed
		// values (eg. 1 = 'a') are compared as text (they are never equal in SQLite)
		lCasted := castedParam(&l)
		rCasted := castedParam(&r)
		if paramKind(&l) == paramKind(&r) {
			l.Identifier = lCasted
			r.Identifier = rCasted
		}
	}

	l.Params = stringifyParams(l.Params)
	r.Params = stringifyParams(r.Params)

	return &l, &r
}

// isPlainParam reports whether the result is a single placeholder param (eg. "{:p1}").
func isPlainParam(result *ResolverResult) bool {
	return len(result.Params) == 1 && plainParamRegex.MatchString(result.Identifier)
}

// plainParamValue returns the value of a plain param result.
func plainParamValue(result *ResolverResult) any {
	m := plainParamRegex.FindStringSubmatch(result.Identifier)
	if len(m) != 2 {
		return nil
	}

	return result.Params[m[1]]
}

// castedParam returns the identifier of a plain param with an explicit
// cast based on its Go value type (the text values are left untouched).
func castedParam(result *ResolverResult) string {
	switch plainParamValue(result).(type) {
	case float64, float32, int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8:
		return result.Identifier + "::double precision"
	case bool:
		return result.Identifier + "::boolean"
	default:
		return result.Identifier
	}
}

// paramKind returns the kind of the plain param value ("number", "bool" or "text").
func paramKind(result *ResolverResult) string {
	switch plainParamValue(result).(type) {
	case float64, float32, int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8:
		return "number"
	case bool:
		return "bool"
	default:
		return "text"
	}
}

// jsonbOperand returns the jsonb equivalent of the provided non-json operand.
func jsonbOperand(result *ResolverResult) string {
	if isPlainParam(result) {
		switch plainParamValue(result).(type) {
		case nil:
			return "NULL"
		case string, []byte:
			return "to_jsonb(" + result.Identifier + "::text)"
		case float64, float32, int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8:
			return "to_jsonb(" + result.Identifier + "::double precision)"
		case bool:
			return "to_jsonb(" + result.Identifier + "::boolean)"
		default:
			return "popgres_jsonb(" + result.Identifier + "::text)"
		}
	}

	switch strings.ToUpper(result.Identifier) {
	case "NULL":
		return "NULL"
	case "TRUE":
		return "'true'::jsonb"
	case "FALSE":
		return "'false'::jsonb"
	case "''", `""`:
		return `'""'::jsonb`
	}

	return "popgres_jsonb(" + result.Identifier + ")"
}

// textOperand returns the text representation of the provided operand identifier
// (the json string values are returned without the quotes).
func textOperand(result *ResolverResult) string {
	if result.JSON {
		return "(" + result.Identifier + " #>> '{}')"
	}

	return "(" + result.Identifier + ")::text"
}

// stringifyParams returns a copy of the provided params with the scalar
// non-string values converted to their text representation.
func stringifyParams(params dbx.Params) dbx.Params {
	if len(params) == 0 {
		return params
	}

	result := make(dbx.Params, len(params))

	for k, v := range params {
		result[k] = stringifyParam(v)
	}

	return result
}

func stringifyParam(v any) any {
	switch val := v.(type) {
	case nil, string, []byte:
		return val
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(val), 'f', -1, 32)
	case int, int64, int32, int16, int8, uint, uint64, uint32, uint16, uint8:
		return fmt.Sprint(val)
	case bool:
		return strconv.FormatBool(val)
	case fmt.Stringer:
		return val.String()
	default:
		return val
	}
}
