package dbtest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func suiteTestCases(t *testing.T) []TestCase {
	t.Helper()
	f, err := ParseTestFile("tests/a.test.hcl", []byte("version = 1\nschema_test \"first\" {}\nschema_test \"second\" {}\nschema_test \"skip\" { skip = \"not available\" }"))
	if err != nil {
		t.Fatal(err)
	}
	return f.Cases()
}

func TestSuiteSelectionAndValidationBeforeExecution(t *testing.T) {
	f, err := ParseTestFile("tests/a.test.hcl", []byte("version = 1\nschema_test \"run\" {}\nschema_test \"skip\" { skip = \"reason\" }"))
	if err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{"[", "tests/*.test.hcl::missing", "tests/*.test.hcl::skip"} {
		if _, err := SelectTests([]TestFile{f}, "schema", filter); err == nil {
			t.Fatalf("invalid selection accepted: %s", filter)
		}
	}
	cases, err := SelectTests([]TestFile{f}, "schema", "tests/*.test.hcl::*")
	if err != nil || len(cases) != 2 {
		t.Fatalf("selection: %v %v", cases, err)
	}
	var called atomic.Int32
	r := SuiteRunner{Parallel: 33, runCase: func(context.Context, TestCase) CaseResult { called.Add(1); return CaseResult{} }}
	if _, err := r.Run(context.Background(), cases); err == nil || called.Load() != 0 {
		t.Fatal("invalid parallelism reached execution")
	}
	r.Parallel = 1
	if _, err := r.Run(context.Background(), []TestCase{{}}); err == nil || called.Load() != 0 {
		t.Fatal("unparsed case reached execution")
	}
}

func TestSuiteParallelResultsKeepDiscoveryOrderAndFirstFailure(t *testing.T) {
	cases := suiteTestCases(t)
	started := make(chan struct{})
	r := SuiteRunner{Parallel: 2, runCase: func(ctx context.Context, c TestCase) CaseResult {
		result := initialCaseResult(c)
		result.Started, result.Executed = true, true
		result.Outcome = "failed"
		if c.Name() == "first" {
			<-started
			result.Primary = &CaseFailure{Kind: "assertion", Code: 5, Message: "assertion failed"}
		} else {
			close(started)
			result.Primary = &CaseFailure{Kind: "migration", Code: 7, Message: "migration failed"}
		}
		return result
	}}
	got, err := r.Run(context.Background(), cases)
	var failure *SuiteError
	if !errors.As(err, &failure) || failure.Failure.Code != 5 || got.Failed != 2 || got.Skipped != 1 || got.Executed != 2 {
		t.Fatalf("results: %#v %v", got, err)
	}
	if got.Cases[0].Name != "first" || got.Cases[1].Name != "second" || got.Cases[2].Outcome != "skipped" {
		t.Fatal("completion order changed report order")
	}
}

func TestSuiteCancellationStopsStartsAndRetainsAllOutcomes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var called atomic.Int32
	r := SuiteRunner{Parallel: 1, runCase: func(ctx context.Context, c TestCase) CaseResult {
		called.Add(1)
		cancel()
		result := initialCaseResult(c)
		result.Started, result.Executed = true, true
		result.Outcome = "failed"
		result.Primary = &CaseFailure{Kind: "canceled", Code: 130, Message: "canceled"}
		return result
	}}
	result, err := r.Run(ctx, suiteTestCases(t))
	if !errors.Is(err, context.Canceled) || called.Load() != 1 || result.NotRun != 1 || result.Skipped != 1 || result.Failed != 1 {
		t.Fatalf("cancellation: %#v %v", result, err)
	}
}

func TestSuiteCleanupDoesNotOverridePrimary(t *testing.T) {
	result := CaseResult{Outcome: "failed", Primary: &CaseFailure{Kind: "assertion", Code: 5}, Cleanup: []CaseFailure{{Kind: "cleanup", Code: 7}, {Kind: "timeout", Code: 124}}}
	if got := caseOutcomeFailure(result); got.Code != 5 {
		t.Fatal("cleanup replaced primary")
	}
	result.Primary = nil
	if got := caseOutcomeFailure(result); got.Code != 7 {
		t.Fatal("cleanup-only must use exit 7")
	}
	if failure := testFailure("exec", Operation{}, context.DeadlineExceeded); failure.Code != 124 {
		t.Fatal("case deadline lost")
	}
	if _, err := (SuiteRunner{Parallel: 1, Timeout: -time.Second}).Run(context.Background(), suiteTestCases(t)); err == nil {
		t.Fatal("negative timeout accepted")
	}
}
