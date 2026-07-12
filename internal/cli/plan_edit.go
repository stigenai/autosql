package cli

import (
	"autosql/pkg/artifact"
	"autosql/pkg/planedit"
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"
)

func runPlanEdit(_ context.Context, args []string, o output) error {
	fs := newFlags("plan edit", o.streams.Err)
	artifactPath := fs.String("artifact", "", "generated artifact file")
	sqlPath := fs.String("sql", "", "edited SQL file")
	editor := fs.String("editor", "", "trusted editor identity")
	reason := fs.String("reason", "", "edit reason")
	output := fs.String("output", "", "edited artifact output file")
	jsonFlag := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *artifactPath == "" || *sqlPath == "" || *editor == "" || *reason == "" || *output == "" {
		return usageError(errors.New("--artifact, --sql, --editor, --reason, and --output required"))
	}
	raw, err := os.ReadFile(*artifactPath)
	if err != nil {
		return &Error{Kind: "validation", Message: "read generated artifact failed", Code: ExitValidation}
	}
	a, err := artifact.Parse(raw)
	if err != nil {
		return &Error{Kind: "validation", Message: "parse generated artifact failed", Code: ExitValidation}
	}
	sql, err := os.ReadFile(*sqlPath)
	if err != nil {
		return &Error{Kind: "validation", Message: "read edited SQL failed", Code: ExitValidation}
	}
	edited, err := planedit.New(raw, a, string(sql), *sqlPath, planedit.Editor{Identity: *editor, At: time.Now().UTC(), Reason: *reason})
	if err != nil {
		return &Error{Kind: "validation", Message: "controlled edit failed", Code: ExitValidation, Cause: err}
	}
	encoded, err := json.Marshal(edited)
	if err != nil {
		return err
	}
	if err = os.WriteFile(*output, encoded, 0600); err != nil {
		return &Error{Kind: "validation", Message: "write edited artifact failed", Code: ExitValidation}
	}
	o.json = *jsonFlag
	return o.success(map[string]any{"status": "edited", "digest": edited.Digest, "output": *output}, edited.Digest)
}
func runPlanReview(_ context.Context, args []string, o output) error {
	fs := newFlags("plan review", o.streams.Err)
	path := fs.String("artifact", "", "artifact file")
	jsonFlag := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *path == "" {
		return usageError(errors.New("--artifact required"))
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		return &Error{Kind: "validation", Message: "read review artifact failed", Code: ExitValidation}
	}
	var e planedit.EditedArtifact
	if err = json.Unmarshal(raw, &e); err != nil {
		return &Error{Kind: "validation", Message: "parse review artifact failed", Code: ExitValidation}
	}
	o.json = *jsonFlag
	return o.success(map[string]any{"digest": e.Digest, "original_plan_digest": e.OriginalPlan.Digest, "candidate_plan_digest": e.CandidatePlan.Digest, "edits": len(e.Provenance)}, e.Digest)
}
