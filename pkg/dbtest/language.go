package dbtest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"autosql/pkg/config"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

const maxTestFileBytes = 1 << 20

// LanguageError reports a bounded diagnostic and the exact source range. It
// deliberately omits expression values, SQL and raw HCL diagnostics.
type LanguageError struct {
	Range   config.SourceRange `json:"range"`
	Message string             `json:"message"`
}

func (e *LanguageError) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.Range.Filename, e.Range.StartLine, e.Range.StartColumn, e.Message)
}

func testRange(at hcl.Range) config.SourceRange {
	return config.SourceRange{Filename: at.Filename, StartLine: at.Start.Line, StartColumn: at.Start.Column, EndLine: at.End.Line, EndColumn: at.End.Column}
}
func languageError(at hcl.Range, message string) error {
	return &LanguageError{Range: testRange(at), Message: message}
}
func languageDiagnostics(diags hcl.Diagnostics, fallback hcl.Range) error {
	for _, diagnostic := range diags {
		if diagnostic.Severity != hcl.DiagError {
			continue
		}
		if diagnostic.Subject != nil {
			fallback = *diagnostic.Subject
		}
		return languageError(fallback, "invalid test syntax or unsupported field")
	}
	return nil
}

// TestFile owns its parsed values. Accessors return independent snapshots.
type TestFile struct {
	filename, digest string
	cases            []TestCase
}

func (f TestFile) Filename() string  { return f.filename }
func (f TestFile) Digest() string    { return f.digest }
func (f TestFile) Cases() []TestCase { return slices.Clone(f.cases) }

// TestCase cannot be constructed with executable operations outside this
// package; parsing is the authority for language validation, not DB access.
type TestCase struct {
	name, kind, skip    string
	at                  config.SourceRange
	timeout             time.Duration
	operations, cleanup []Operation
	migrationDigest     string
	migrations          map[int][]testMigration
}

func (c TestCase) Name() string              { return c.name }
func (c TestCase) Kind() string              { return c.kind }
func (c TestCase) Range() config.SourceRange { return c.at }
func (c TestCase) SkipReason() string        { return c.skip }
func (c TestCase) Timeout() time.Duration    { return c.timeout }
func (c TestCase) Operations() []Operation   { return cloneOperations(c.operations) }
func (c TestCase) Cleanup() []Operation      { return cloneOperations(c.cleanup) }
func (c TestCase) MigrationDigest() string   { return c.migrationDigest }

// Operation is a detached view of a parsed operation. Version transitions are
// flattened in declaration order, immediately before their nested seed/checks.
type Operation struct {
	Kind, Name, SQL, Version, SQLState string
	Range                              config.SourceRange
	Params                             []Scalar
	Equals                             Scalar
	NonTransactional                   bool
}

func cloneOperations(operations []Operation) []Operation {
	result := slices.Clone(operations)
	for i := range result {
		result[i].Params = slices.Clone(result[i].Params)
	}
	return result
}

// ParseTestFile is pure: no filesystem, environment, secret, network or process
// access. Only literal values and version-1 blocks are accepted.
func ParseTestFile(filename string, data []byte) (TestFile, error) {
	if err := boundTestSyntax(filename, data); err != nil {
		return TestFile{}, err
	}
	parsed, diags := hclsyntax.ParseConfig(data, filename, hcl.InitialPos)
	if err := languageDiagnostics(diags, hcl.Range{Filename: filename, Start: hcl.InitialPos}); err != nil {
		return TestFile{}, err
	}
	body := parsed.Body.(*hclsyntax.Body)
	content, diags := body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "version", Required: true}}, Blocks: []hcl.BlockHeaderSchema{{Type: "schema_test", LabelNames: []string{"name"}}, {Type: "migration_test", LabelNames: []string{"name"}}}})
	if err := languageDiagnostics(diags, body.Range()); err != nil {
		return TestFile{}, err
	}
	version, err := testScalar(content.Attributes["version"].Expr)
	if err != nil {
		return TestFile{}, err
	}
	if version != IntegerScalar(1) {
		return TestFile{}, languageError(content.Attributes["version"].Range, "test files require integer version 1")
	}
	cases, err := parseTestCases(content.Blocks, body.Range())
	if err != nil {
		return TestFile{}, err
	}
	digest := sha256.Sum256(data)
	return TestFile{filename: filename, digest: hex.EncodeToString(digest[:]), cases: cases}, nil
}

func parseTestCases(blocks hcl.Blocks, at hcl.Range) ([]TestCase, error) {
	if len(blocks) == 0 {
		return nil, languageError(at, "test file requires at least one case")
	}
	seen := map[string]bool{}
	result := make([]TestCase, 0, len(blocks))
	for _, block := range blocks {
		name, err := testBlockName(block)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, languageError(block.LabelRanges[0], "duplicate test case name")
		}
		seen[name] = true
		c, err := parseTestCase(block, name)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, nil
}

func boundTestSyntax(filename string, data []byte) error {
	at := hcl.Range{Filename: filename, Start: hcl.InitialPos, End: hcl.InitialPos}
	if len(data) > maxTestFileBytes {
		return languageError(at, "test file exceeds one MiB")
	}
	tokens, diags := hclsyntax.LexConfig(data, filename, hcl.InitialPos)
	if err := languageDiagnostics(diags, at); err != nil {
		return err
	}
	depth := 0
	for i, token := range tokens {
		switch token.Type {
		case hclsyntax.TokenOBrace, hclsyntax.TokenOBrack, hclsyntax.TokenOParen, hclsyntax.TokenOQuote, hclsyntax.TokenOHeredoc, hclsyntax.TokenTemplateInterp, hclsyntax.TokenTemplateControl:
			depth++
		case hclsyntax.TokenCBrace, hclsyntax.TokenCBrack, hclsyntax.TokenCParen, hclsyntax.TokenCQuote, hclsyntax.TokenCHeredoc, hclsyntax.TokenTemplateSeqEnd:
			depth--
		}
		if i >= 65536 || depth > 32 {
			return languageError(token.Range, "test structure exceeds parsing limits")
		}
	}
	return nil
}
