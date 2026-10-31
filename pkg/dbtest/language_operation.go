package dbtest

import (
	"regexp"

	"github.com/hashicorp/hcl/v2"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func parseTestOperation(block *hcl.Block) (Operation, error) {
	op := Operation{Kind: block.Type, Range: testRange(block.DefRange)}
	if len(block.Labels) > 0 {
		name, err := testBlockName(block)
		if err != nil {
			return op, err
		}
		op.Name = name
	}
	attributes := []hcl.AttributeSchema{{Name: "sql", Required: true}, {Name: "params"}}
	if block.Type == "assert" {
		attributes = append(attributes, hcl.AttributeSchema{Name: "equals", Required: true})
	}
	if block.Type == "expect_error" {
		attributes = append(attributes, hcl.AttributeSchema{Name: "sqlstate", Required: true}, hcl.AttributeSchema{Name: "transaction"})
	}
	content, diags := block.Body.Content(&hcl.BodySchema{Attributes: attributes})
	if err := languageDiagnostics(diags, block.DefRange); err != nil {
		return op, err
	}
	if err := parseOperationSQL(&op, content.Attributes); err != nil {
		return op, err
	}
	if block.Type == "assert" {
		value, err := testScalar(content.Attributes["equals"].Expr)
		op.Equals = value
		return op, err
	}
	if block.Type == "expect_error" {
		err := parseExpectedError(&op, content.Attributes)
		return op, err
	}
	return op, nil
}

func parseOperationSQL(op *Operation, attributes hcl.Attributes) error {
	query, err := testString(attributes["sql"])
	if err != nil {
		return err
	}
	statements, err := pg_query.SplitWithScanner(query, true)
	if err != nil || len(statements) != 1 {
		return languageError(attributes["sql"].Range, "operation requires exactly one SQL statement")
	}
	op.SQL = query
	params, err := testParameters(attributes["params"])
	op.Params = params
	return err
}

var expectedStatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

func validExpectedState(state string) bool {
	if !expectedStatePattern.MatchString(state) {
		return false
	}
	switch state[:2] {
	case "00", "01", "02", "08", "57", "58":
		return false
	default:
		return true
	}
}

func parseExpectedError(op *Operation, attributes hcl.Attributes) error {
	state, err := testString(attributes["sqlstate"])
	if err != nil {
		return err
	}
	if !validExpectedState(state) {
		return languageError(attributes["sqlstate"].Range, "sqlstate requires an exact error code; success, connection, cancellation and system errors are not expected failures")
	}
	op.SQLState = state
	if attribute := attributes["transaction"]; attribute != nil {
		value, err := testString(attribute)
		if err != nil {
			return err
		}
		if value != "required" && value != "forbidden" {
			return languageError(attribute.Range, "transaction must be required or forbidden")
		}
		op.NonTransactional = value == "forbidden"
	}
	return nil
}
