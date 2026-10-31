package dbtest

import (
	"context"
	"errors"
	"path"
	"slices"
	"time"
)

func (r SuiteRunner) executeCase(parent context.Context, c TestCase) (result CaseResult) {
	if r.runCase != nil {
		return r.runCase(parent, c)
	}
	result = initialCaseResult(c)
	started := time.Now()
	defer func() { result.DurationMillis = time.Since(started).Milliseconds() }()
	if err := r.verify(parent); err != nil {
		result.fail(testFailure("inputs", Operation{Range: c.at}, err))
		return result
	}
	result.Started = true
	target, err := r.Factory.Create(parent, c.name)
	if err != nil {
		recordProvisionFailure(&result, c, err)
		return result
	}
	if target == nil || target.conn == nil || target.lease == nil {
		result.fail(testFailure("provision", Operation{Range: c.at}, ErrLease))
		return result
	}
	ctx, cancel := context.WithTimeout(parent, r.caseTimeout(c))
	defer cancel()
	defer r.cleanCase(ctx, c, target, &result)
	result.PostgresMajor = target.PostgresMajor()
	result.Executed = true
	if c.kind == "schema" {
		if !r.prepareSchemaCase(ctx, c, target, &result) {
			return result
		}
	}
	r.executeOperations(ctx, c, target, &result)
	return result
}

func (r SuiteRunner) prepareSchemaCase(ctx context.Context, c TestCase, target *DisposableTarget, result *CaseResult) bool {
	if !executeSchemaSetup(ctx, c, target, result) {
		return false
	}
	primary, cleanup := target.prepareSchema(ctx, r.Schema)
	result.addCleanup(cleanup, c.at)
	if primary != nil {
		result.fail(*primary)
	}
	return primary == nil && len(cleanup) == 0
}

func executeSchemaSetup(ctx context.Context, c TestCase, target *DisposableTarget, result *CaseResult) bool {
	for _, op := range c.operations {
		if op.Kind != "setup" {
			break
		}
		if err := executeTestOperation(ctx, target, op); err != nil {
			recordOperationError(result, op, err)
			return false
		}
	}
	return true
}

func (r SuiteRunner) caseTimeout(c TestCase) time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 5 * time.Minute
}

func (r SuiteRunner) verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.VerifyInputs == nil {
		return nil
	}
	if err := r.VerifyInputs(ctx); err != nil {
		return errors.Join(errTestInputs, err)
	}
	return nil
}

func (r SuiteRunner) executeOperations(ctx context.Context, c TestCase, target *DisposableTarget, result *CaseResult) {
	for i, op := range c.operations {
		if schemaSetupOperation(c, op) {
			continue
		}
		if err := ctx.Err(); err != nil {
			result.fail(testFailure(op.Kind, op, err))
			return
		}
		if op.Kind == "version" {
			if !r.executeVersion(ctx, c, i, op, target, result) {
				return
			}
			continue
		}
		if op.Kind == "assert" || op.Kind == "expect_error" {
			result.Assertions++
		}
		if err := executeTestOperation(ctx, target, op); err != nil {
			recordOperationError(result, op, err)
			return
		}
	}
	if err := ctx.Err(); err != nil {
		result.fail(testFailure("case", Operation{Range: c.at}, err))
		return
	}
	result.Outcome = "passed"
}

func schemaSetupOperation(c TestCase, op Operation) bool {
	return c.kind == "schema" && op.Kind == "setup"
}

func executeTestOperation(ctx context.Context, target *DisposableTarget, op Operation) error {
	args := scalarParameters(op.Params)
	switch op.Kind {
	case "assert":
		observed, err := target.QueryScalar(ctx, op.SQL, args...)
		if err != nil {
			return err
		}
		if observed != op.Equals {
			return ErrScalarMismatch
		}
		return nil
	case "expect_error":
		return target.ExpectError(ctx, op.SQL, op.SQLState, op.NonTransactional, args...)
	default:
		return target.ExecParameters(ctx, op.SQL, args...)
	}
}

func (r SuiteRunner) executeVersion(ctx context.Context, c TestCase, index int, op Operation, target *DisposableTarget, result *CaseResult) bool {
	if err := r.verify(ctx); err != nil {
		result.fail(testFailure("inputs", op, err))
		return false
	}
	for _, migration := range c.migrations[index] {
		statements := slices.Clone(migration.statements)
		for i := range statements {
			statements[i].Position.File = path.Join(r.MigrationDirectory, statements[i].Position.File)
		}
		batch := sqlBatch{statements: statements, transactional: migration.transaction != "forbidden"}
		primary, cleanup := target.executeBatch(ctx, batch, "migration")
		result.addCleanup(cleanup, op.Range)
		if primary != nil {
			result.fail(*primary)
			return false
		}
		if len(cleanup) > 0 {
			return false
		}
	}
	result.Versions = append(result.Versions, op.Version)
	return true
}

func (r SuiteRunner) cleanCase(ctx context.Context, c TestCase, target *DisposableTarget, result *CaseResult) {
	for i := len(c.cleanup) - 1; i >= 0; i-- {
		op := c.cleanup[i]
		cleanupCtx, cancel := freshSetupContext(ctx, r.CleanupTimeout)
		err := target.executeCleanup(cleanupCtx, op)
		cancel()
		if err != nil {
			appendFixtureCleanupFailures(result, op, err)
		}
	}
	cleanupCtx, cancel := freshSetupContext(ctx, r.CleanupTimeout)
	err := target.Close(cleanupCtx)
	cancel()
	if err != nil {
		result.Cleanup = append(result.Cleanup, testFailure("cleanup", Operation{Range: c.at}, err))
	}
	info := target.Info()
	result.Lease = &info
	if len(result.Cleanup) > 0 {
		result.Outcome = "failed"
	}
}

func appendFixtureCleanupFailures(result *CaseResult, op Operation, err error) {
	var cleanup *OperationCleanupError
	if errors.As(err, &cleanup) {
		result.Cleanup = append(result.Cleanup, testFailure("cleanup_connection", op, cleanup.Cleanup))
		err = cleanup.Primary
	}
	if err != nil {
		result.Cleanup = append(result.Cleanup, testFailure("cleanup", op, err))
	}
}

func (r *CaseResult) fail(failure CaseFailure) {
	if failure.Range.Filename == "" {
		failure.Range = r.Range
	}
	r.Outcome = "failed"
	r.Primary = &failure
}

func recordOperationError(result *CaseResult, op Operation, err error) {
	var cleanup *OperationCleanupError
	if errors.As(err, &cleanup) {
		result.Cleanup = append(result.Cleanup, testFailure("operation_cleanup", op, cleanup.Cleanup))
		err = cleanup.Primary
	}
	if err != nil {
		result.fail(testFailure(op.Kind, op, err))
	} else {
		result.Outcome = "failed"
	}
}

func recordProvisionFailure(result *CaseResult, c TestCase, err error) {
	result.fail(testFailure("provision", Operation{Range: c.at}, err))
	var factory *FactoryError
	if !errors.As(err, &factory) || factory.LeasePath == "" {
		return
	}
	info, inspectErr := InspectLease(factory.LeasePath)
	if inspectErr != nil {
		info = LeaseInfo{Path: factory.LeasePath, State: "unknown"}
	}
	result.Lease = &info
	for range info.CleanupErrors {
		result.Cleanup = append(result.Cleanup, CaseFailure{Kind: "cleanup", Code: 7, Stage: "provision_cleanup", Range: c.at, Message: "partial allocation cleanup failed; inspect the retained lease"})
	}
}
