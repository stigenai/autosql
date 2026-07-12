package cli

import (
	"autosql/pkg/migrate/down"
	"context"
	"errors"
	"fmt"
	"strings"
)

type DownService interface {
	PlanDown(context.Context, string) (down.DownPlan, error)
	ApplyDown(context.Context, down.DownPlan) (string, error)
}

func runMigrateDown(ctx context.Context, args []string, o output, s DownService) error {
	fs := newFlags("migrate down", o.streams.Err)
	to := fs.String("to", "", "prior revision")
	dry := fs.Bool("dry-run", false, "plan without mutation")
	jsonFlag := fs.Bool("json", false, "JSON")
	if e := fs.Parse(args); e != nil {
		return usageError(e)
	}
	if fs.NArg() != 0 || *to == "" || s == nil {
		return usageError(errors.New("--to and trusted down service required"))
	}
	o.json = *jsonFlag
	p, e := s.PlanDown(ctx, *to)
	if e != nil {
		return &Error{Kind: "migration", Message: "controlled down planning refused", Code: ExitMigration, Cause: e, RecoveryGuidance: "reconcile dirty or uncertain revision state before retry"}
	}
	if *dry {
		var b strings.Builder
		fmt.Fprintf(&b, "down plan %s to %s\n", p.Digest, p.TargetVersion)
		for _, impact := range p.Impacts {
			kind := "change"
			if impact.Destructive {
				kind = "DESTRUCTIVE"
			}
			fmt.Fprintf(&b, "- %s %s %s\n", kind, impact.Operation, impact.Object)
			for _, condition := range impact.Preconditions {
				fmt.Fprintf(&b, "  precondition: %s\n", condition)
			}
		}
		return o.success(p, strings.TrimSpace(b.String()))
	}
	status, e := s.ApplyDown(ctx, p)
	if e != nil {
		return &Error{Kind: "migration", Message: "controlled down apply failed", Code: ExitMigration, Cause: e, Status: status, RecoveryGuidance: "inspect reversal event and reconcile before retry"}
	}
	return o.success(map[string]any{"status": status, "down_plan_digest": p.Digest}, status)
}
