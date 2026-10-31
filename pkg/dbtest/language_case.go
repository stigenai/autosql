package dbtest

import (
	"strings"
	"time"

	"autosql/pkg/migrate"
	"github.com/hashicorp/hcl/v2"
)

func operationHeaders(seed bool) []hcl.BlockHeaderSchema {
	result := []hcl.BlockHeaderSchema{{Type: "setup"}, {Type: "fixture"}, {Type: "exec"}, {Type: "assert", LabelNames: []string{"name"}}, {Type: "expect_error", LabelNames: []string{"name"}}, {Type: "cleanup"}}
	if seed {
		result = append(result, hcl.BlockHeaderSchema{Type: "seed"})
	}
	return result
}

func parseTestCase(block *hcl.Block, name string) (TestCase, error) {
	c := TestCase{name: name, kind: strings.TrimSuffix(block.Type, "_test"), at: testRange(block.DefRange)}
	headers := operationHeaders(false)
	if c.kind == "migration" {
		headers = append(headers, hcl.BlockHeaderSchema{Type: "version", LabelNames: []string{"version"}})
	}
	content, diags := block.Body.Content(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "skip"}, {Name: "timeout"}}, Blocks: headers})
	if err := languageDiagnostics(diags, block.DefRange); err != nil {
		return TestCase{}, err
	}
	if err := parseCaseOptions(&c, content.Attributes); err != nil {
		return TestCase{}, err
	}
	if err := parseCaseOperations(&c, content.Blocks); err != nil {
		return TestCase{}, err
	}
	return c, nil
}

func parseCaseOptions(c *TestCase, attributes hcl.Attributes) error {
	if attribute := attributes["skip"]; attribute != nil {
		value, err := testString(attribute)
		if err != nil {
			return err
		}
		c.skip = value
	}
	if attribute := attributes["timeout"]; attribute != nil {
		value, err := testString(attribute)
		if err != nil {
			return err
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 || duration > 24*time.Hour {
			return languageError(attribute.Range, "timeout must be positive and at most 24h")
		}
		c.timeout = duration
	}
	return nil
}

func parseCaseOperations(c *TestCase, blocks hcl.Blocks) error {
	var prior *migrate.Version
	for _, block := range blocks {
		if block.Type == "version" {
			version, err := parseVersionOperations(c, block, prior)
			if err != nil {
				return err
			}
			prior = &version
			continue
		}
		if err := appendTestOperation(c, block); err != nil {
			return err
		}
	}
	if c.kind == "migration" && prior == nil {
		return &LanguageError{Range: c.at, Message: "migration test requires at least one version transition"}
	}
	return validateSchemaSetup(*c)
}

func validateSchemaSetup(c TestCase) error {
	if c.kind != "schema" {
		return nil
	}
	started := false
	for _, op := range c.operations {
		if op.Kind == "setup" && started {
			return &LanguageError{Range: op.Range, Message: "schema setup must precede fixtures and checks"}
		}
		started = started || op.Kind != "setup"
	}
	return nil
}

func parseVersionOperations(c *TestCase, block *hcl.Block, prior *migrate.Version) (migrate.Version, error) {
	version, err := migrate.ParseVersion(block.Labels[0])
	if err != nil {
		return version, languageError(block.LabelRanges[0], "invalid migration version")
	}
	if prior != nil && version.Compare(*prior) <= 0 {
		return version, languageError(block.LabelRanges[0], "migration versions must increase strictly")
	}
	content, diags := block.Body.Content(&hcl.BodySchema{Blocks: operationHeaders(true)})
	if err := languageDiagnostics(diags, block.DefRange); err != nil {
		return version, err
	}
	c.operations = append(c.operations, Operation{Kind: "version", Version: version.String(), Range: testRange(block.DefRange)})
	for _, child := range content.Blocks {
		if err := appendTestOperation(c, child); err != nil {
			return version, err
		}
	}
	return version, nil
}

func appendTestOperation(c *TestCase, block *hcl.Block) error {
	operation, err := parseTestOperation(block)
	if err != nil {
		return err
	}
	if operation.Kind == "cleanup" {
		c.cleanup = append(c.cleanup, operation)
	} else {
		c.operations = append(c.operations, operation)
	}
	return nil
}
