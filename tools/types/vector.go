package types

import (
	"database/sql/driver"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// popgres: the value of the pgvector "vector" type.

// Vector defines a pgvector vector value (a list of float32 numbers).
//
// The zero (empty) Vector is stored as NULL and serialized as JSON null.
type Vector []float32

// String returns the pgvector text representation of the vector (eg. "[1,2.5,3]").
func (v Vector) String() string {
	var b strings.Builder

	b.WriteByte('[')
	for i, n := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(n), 'g', -1, 32))
	}
	b.WriteByte(']')

	return b.String()
}

// MarshalJSON implements the [json.Marshaler] interface.
func (v Vector) MarshalJSON() ([]byte, error) {
	if len(v) == 0 {
		return []byte("null"), nil
	}

	return []byte(v.String()), nil
}

// UnmarshalJSON implements the [json.Unmarshaler] interface.
func (v *Vector) UnmarshalJSON(data []byte) error {
	return v.Scan(data)
}

// Value implements the [driver.Valuer] interface.
func (v Vector) Value() (driver.Value, error) {
	if len(v) == 0 {
		return nil, nil
	}

	return v.String(), nil
}

// Scan implements [sql.Scanner] interface to scan the provided value into the current Vector.
//
// It accepts the pgvector text representation, a json array (as string or bytes)
// and a slice of numbers.
func (v *Vector) Scan(value any) error {
	switch val := value.(type) {
	case nil:
		*v = nil
		return nil
	case Vector:
		*v = val
		return nil
	case []float32:
		*v = val
		return nil
	case []float64:
		result := make(Vector, len(val))
		for i, n := range val {
			result[i] = float32(n)
		}
		*v = result
		return v.validate()
	case []any:
		result := make(Vector, len(val))
		for i, item := range val {
			n, err := vectorNumber(item)
			if err != nil {
				return fmt.Errorf("invalid vector item %d: %w", i, err)
			}
			result[i] = n
		}
		*v = result
		return v.validate()
	case []byte:
		return v.scanString(string(val))
	case string:
		return v.scanString(val)
	default:
		return fmt.Errorf("unsupported vector value type %T", value)
	}
}

func (v *Vector) scanString(str string) error {
	str = strings.TrimSpace(str)

	if str == "" || str == "null" || str == "[]" {
		*v = nil
		return nil
	}

	if !strings.HasPrefix(str, "[") || !strings.HasSuffix(str, "]") {
		return errors.New("invalid vector format, expected an array of numbers")
	}

	// a json array or the pgvector text format (which has the same syntax)
	raw := []any{}
	if err := json.Unmarshal([]byte(str), &raw); err != nil {
		return fmt.Errorf("invalid vector format: %w", err)
	}

	return v.Scan(raw)
}

func (v Vector) validate() error {
	for i, n := range v {
		if math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) {
			return fmt.Errorf("invalid vector item %d: the numbers must be finite", i)
		}
	}
	return nil
}

func vectorNumber(item any) (float32, error) {
	switch n := item.(type) {
	case float64:
		if n > math.MaxFloat32 || n < -math.MaxFloat32 {
			return 0, errors.New("the number is out of the float32 range")
		}
		return float32(n), nil
	case float32:
		return n, nil
	case int:
		return float32(n), nil
	case int64:
		return float32(n), nil
	default:
		return 0, fmt.Errorf("expected a number, got %T", item)
	}
}
