package dbtest

import "errors"

var (
	ErrScalarCardinality = errors.New("assertion query must return exactly one row and one column")
	ErrScalarType        = errors.New("assertion requires a PostgreSQL integer, text, boolean or null scalar")
	ErrScalarMismatch    = errors.New("assertion scalar does not match the expected type and value")
	ErrExpectedError     = errors.New("operation did not return the expected SQLSTATE")
)

// Scalar is a comparable immutable typed value. The zero value represents SQL
// NULL. Values are never formatted into assertion failure messages.
type Scalar struct {
	kind    string
	integer int64
	text    string
	boolean bool
}

func IntegerScalar(value int64) Scalar { return Scalar{kind: "integer", integer: value} }
func StringScalar(value string) Scalar { return Scalar{kind: "string", text: value} }
func BooleanScalar(value bool) Scalar  { return Scalar{kind: "boolean", boolean: value} }
func NullScalar() Scalar               { return Scalar{} }
func (s Scalar) Kind() string {
	if s.kind == "" {
		return "null"
	}
	return s.kind
}

// Value returns a value suitable for a bound PostgreSQL parameter, never SQL
// interpolation. No float conversion is used for signed 64-bit integers.
func (s Scalar) Value() any {
	switch s.kind {
	case "integer":
		return s.integer
	case "string":
		return s.text
	case "boolean":
		return s.boolean
	default:
		return nil
	}
}

func scalarParameters(values []Scalar) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value.Value()
	}
	return args
}
