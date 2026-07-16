package postgres

import (
	"fmt"
	"regexp"
	"strings"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

var routineSecretLiteral = regexp.MustCompile(`(?i)(?:password|secret|token|credential)\s*(?::=|=>|=)\s*'(?:''|[^'])*'`)
var privilegedRoutineSource = regexp.MustCompile(`(?i)\b(?:copy\s+[^;]+\s+program|pg_read_file|pg_read_binary_file|pg_write_file|lo_import|dblink_connect|alter\s+role|create\s+extension)\b`)
var procedureTransactionControl = regexp.MustCompile(`(?i)\b(?:commit|rollback|start\s+transaction)\b`)

type parsedRoutineSource struct {
	statement *pg_query.CreateFunctionStmt
	SQL       string
}

// validateRoutineSource enforces the executable-source trust boundary. A
// digest in a document describes content; only the render option proves that
// the caller reviewed and authorized that exact normalized content.
func validateRoutineSource(resource schema.Resource, options map[string]string) (parsedRoutineSource, error) {
	values := spec(resource)
	definition := stringValue(values, "definition")
	if definition == "" {
		return parsedRoutineSource{}, unsupported(resource, "routine definition is required")
	}
	digest := stringValue(values, "body_digest")
	actualDigest := routineDefinitionDigest(definition)
	if digest == "" || digest != actualDigest {
		return parsedRoutineSource{}, unsupported(resource, "routine body digest does not match the normalized definition")
	}
	reviewed := false
	for _, candidate := range strings.Split(options["reviewed_routine_digests"], ",") {
		reviewed = reviewed || strings.TrimSpace(candidate) == digest
	}
	if !reviewed {
		return parsedRoutineSource{}, unsupported(resource, "routine source is not bound to a reviewed_routine_digests authorization")
	}
	parsed, err := pg_query.Parse(definition)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 {
		return parsedRoutineSource{}, unsupported(resource, "routine source must parse as exactly one CREATE FUNCTION or CREATE PROCEDURE statement")
	}
	statement := parsed.Stmts[0].GetStmt().GetCreateFunctionStmt()
	if statement == nil {
		return parsedRoutineSource{}, unsupported(resource, "routine source must be one CREATE statement")
	}
	if statement.GetIsProcedure() != (resource.Kind == schema.KindProcedure) {
		return parsedRoutineSource{}, unsupported(resource, "routine source kind does not match its resource kind")
	}
	nameParts := statement.GetFuncname()
	if len(nameParts) != 2 || nameParts[0].GetString_().GetSval() != resource.Name.Schema || nameParts[1].GetString_().GetSval() != stringValue(values, "name") {
		return parsedRoutineSource{}, unsupported(resource, "routine source identity does not match its resource")
	}
	language := strings.ToLower(stringValue(values, "language"))
	if language != "sql" && language != "plpgsql" && !enabled(options, "allow_unsafe_routine_languages", false) {
		return parsedRoutineSource{}, unsupported(resource, fmt.Sprintf("routine language %q requires allow_unsafe_routine_languages=true", language))
	}
	if boolValue(values, "security_definer") && !safeRoutineSearchPath(stringSlice(values, "configuration")) {
		return parsedRoutineSource{}, unsupported(resource, "SECURITY DEFINER routine requires a fixed search_path beginning with pg_catalog")
	}
	if routineSecretLiteral.MatchString(definition) {
		return parsedRoutineSource{}, unsupported(resource, "routine source contains a secret-shaped literal")
	}
	if privilegedRoutineSource.MatchString(definition) && !enabled(options, "allow_privileged_routines", false) {
		return parsedRoutineSource{}, unsupported(resource, "privileged routine source requires allow_privileged_routines=true")
	}
	if resource.Kind == schema.KindProcedure && procedureTransactionControl.MatchString(definition) && !enabled(options, "allow_transaction_control_procedures", false) {
		return parsedRoutineSource{}, unsupported(resource, "procedure transaction control requires allow_transaction_control_procedures=true")
	}
	return parsedRoutineSource{statement: statement, SQL: definition}, nil
}

func safeRoutineSearchPath(configuration []string) bool {
	for _, setting := range configuration {
		parts := strings.SplitN(setting, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(strings.ToLower(parts[0])) != "search_path" {
			continue
		}
		first := strings.Trim(strings.TrimSpace(strings.Split(parts[1], ",")[0]), `"'`)
		return first == "pg_catalog"
	}
	return false
}

func routineSignature(resource schema.Resource) (string, error) {
	values := spec(resource)
	name := stringValue(values, "name")
	arguments := stringValue(values, "identity_arguments")
	if name == "" {
		return "", unsupported(resource, "routine base name is required")
	}
	signature := quote(resource.Name.Schema) + "." + quote(name) + "(" + arguments + ")"
	keyword := "FUNCTION"
	if resource.Kind == schema.KindProcedure {
		keyword = "PROCEDURE"
	}
	parsed, err := pg_query.Parse("DROP " + keyword + " " + signature)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 || parsed.Stmts[0].GetStmt().GetDropStmt() == nil {
		return "", unsupported(resource, "routine identity arguments are outside the canonical grammar")
	}
	return signature, nil
}

func routineKeyword(resource schema.Resource) string {
	if resource.Kind == schema.KindProcedure {
		return "PROCEDURE"
	}
	return "FUNCTION"
}
