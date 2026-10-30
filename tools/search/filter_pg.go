package search

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Robernetes/popgres/tools/types"
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
//     json.a = 1 compares numbers and json.a = 'x' compares strings;
//   - the bool literals compared to a non-bool operand (ResolverResult.Type)
//     are converted to 1/0 as in SQLite (eg. title = true is the same as title = '1').

// ValueType defines the SQL type hint of a resolved operand.
type ValueType string

const (
	ValueTypeUnknown ValueType = ""
	ValueTypeText    ValueType = "text"
	ValueTypeNumber  ValueType = "number"
	ValueTypeBool    ValueType = "bool"
	ValueTypeDate    ValueType = "date"

	// ValueTypeTextSearch is the tsvector column of a "search" field
	// (its text search configuration is in ResolverResult.TextSearchConfig).
	ValueTypeTextSearch ValueType = "tsvector"
)

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
	case isBoolLiteral(&l) && !isBoolLiteral(&r):
		normalizeBoolLiteral(&l, &r)
	case isBoolLiteral(&r) && !isBoolLiteral(&l):
		normalizeBoolLiteral(&r, &l)
	case !l.JSON && !r.JSON && isPlainParam(&l) && !isPlainParam(&r):
		normalizeParamOperand(&l, &r)
	case !l.JSON && !r.JSON && isPlainParam(&r) && !isPlainParam(&l):
		normalizeParamOperand(&r, &l)
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

// isBoolLiteral reports whether the result is a TRUE or FALSE literal.
func isBoolLiteral(result *ResolverResult) bool {
	return strings.EqualFold(result.Identifier, "TRUE") || strings.EqualFold(result.Identifier, "FALSE")
}

// normalizeBoolLiteral converts the bool literal operand to the
// SQLite equivalent value based on the type of the other operand.
//
// The other operand is expected to be a non-json value.
func normalizeBoolLiteral(literal, other *ResolverResult) {
	num := "0"
	if strings.EqualFold(literal.Identifier, "TRUE") {
		num = "1"
	}

	switch operandType(other) {
	case ValueTypeNumber:
		literal.Identifier = num
		if isPlainParam(other) {
			other.Identifier = castedParam(other)
		}
	case ValueTypeText:
		literal.Identifier = "'" + num + "'"
	case ValueTypeDate:
		// SQLite stores the dates as text
		literal.Identifier = "'" + num + "'"
		other.Identifier = "(" + other.Identifier + ")::text"
	case ValueTypeBool:
		if isPlainParam(other) {
			other.Identifier = castedParam(other)
		}
	}
}

// normalizeParamOperand ensures that the plain param value is compatible
// with the type of the other (non-json) operand.
//
// Postgres infers the type of the text params from the other operand and fails
// for invalid values (eg. 'abc' for a number column), while SQLite compares them
// as text. For such values both operands are compared as text.
func normalizeParamOperand(param, other *ResolverResult) {
	value := plainParamValue(param)
	kind := paramKind(param)

	switch other.Type {
	case ValueTypeNumber:
		switch {
		case kind == "number":
			// the operand could be an integer expression (eg. a json array length)
			param.Identifier = castedParam(param)
		case kind == "bool":
			param.Identifier = "(" + castedParam(param) + ")::int"
		case !isNumericText(value):
			other.Identifier = "(" + other.Identifier + ")::text"
		}
	case ValueTypeBool:
		switch {
		case kind == "bool":
			param.Identifier = castedParam(param)
		case kind == "number":
			// SQLite stores the bool values as 0 or 1
			param.Identifier = castedParam(param)
			other.Identifier = "(" + other.Identifier + ")::int"
		case !isBoolText(value):
			other.Identifier = "(" + other.Identifier + ")::text"
		}
	case ValueTypeDate:
		if kind != "text" || !isDateText(value) {
			other.Identifier = "(" + other.Identifier + ")::text"
		}
	}
}

func isNumericText(value any) bool {
	str, ok := value.(string)
	if !ok {
		return false
	}

	_, err := strconv.ParseFloat(strings.TrimSpace(str), 64)

	return err == nil
}

func isBoolText(value any) bool {
	str, ok := value.(string)
	if !ok {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(str)) {
	case "true", "false", "t", "f", "1", "0", "yes", "no", "y", "n", "on", "off":
		return true
	}

	return false
}

func isDateText(value any) bool {
	var str string

	switch v := value.(type) {
	case string:
		str = v
	case fmt.Stringer:
		str = v.String()
	default:
		return false
	}

	if strings.TrimSpace(str) == "" {
		return false
	}

	dt, _ := types.ParseDateTime(str)

	return !dt.IsZero()
}

// operandType returns the type hint of the provided operand
// (for the plain params it is based on their Go value type).
func operandType(result *ResolverResult) ValueType {
	if result.Type != ValueTypeUnknown {
		return result.Type
	}

	if isPlainParam(result) {
		switch paramKind(result) {
		case "number":
			return ValueTypeNumber
		case "bool":
			return ValueTypeBool
		default:
			return ValueTypeText
		}
	}

	return ValueTypeUnknown
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
