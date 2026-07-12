package cli

import (
	"autosql/pkg/artifact"
	"autosql/pkg/planedit"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"time"
)

func runPlanEdit(_ context.Context, args []string, o output, service PlanEditService) error {
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
	if service == nil {
		return &Error{Kind: "validation", Message: "trusted production verification is required for editing", Code: ExitValidation}
	}
	if *editor != service.TrustedEditor() {
		return &Error{Kind: "validation", Message: "editor identity is not trusted", Code: ExitValidation}
	}
	if err = service.VerifyOriginal(a); err != nil {
		return &Error{Kind: "validation", Message: "original artifact trust verification failed", Code: ExitValidation}
	}
	sql, err := os.ReadFile(*sqlPath)
	if err != nil {
		return &Error{Kind: "validation", Message: "read edited SQL failed", Code: ExitValidation}
	}
	edited, err := planedit.New(raw, a, string(sql), *sqlPath, planedit.Editor{Identity: service.TrustedEditor(), At: time.Now().UTC(), Reason: *reason})
	if err != nil {
		return &Error{Kind: "validation", Message: "controlled edit failed", Code: ExitValidation, Cause: err}
	}
	encoded, err := json.Marshal(edited)
	if err != nil {
		return err
	}
	if err = atomicCreate(*output, encoded); err != nil {
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&e); err != nil {
		return &Error{Kind: "validation", Message: "parse review artifact failed", Code: ExitValidation}
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF || e.Validate() != nil {
		return &Error{Kind: "validation", Message: "invalid review artifact", Code: ExitValidation}
	}
	o.json = *jsonFlag
	return o.success(map[string]any{"digest": e.Digest, "original_plan_digest": e.OriginalPlan.Digest, "candidate_plan_digest": e.CandidatePlan.Digest, "edits": len(e.Provenance)}, e.Digest)
}
func runPlanRevalidate(ctx context.Context, args []string, o output, s PlanEditService) error {
	fs := newFlags("plan revalidate", o.streams.Err)
	draft := fs.String("draft", "", "validated edit draft")
	out := fs.String("output", "", "attested output")
	jsonFlag := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if s == nil || *draft == "" || *out == "" {
		return usageError(errors.New("configured plan editing plus --draft and --output required"))
	}
	raw, err := os.ReadFile(*draft)
	if err != nil {
		return &Error{Kind: "validation", Message: "read edit draft failed", Code: ExitValidation}
	}
	var e planedit.EditedArtifact
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&e); err != nil || e.Validate() != nil {
		return &Error{Kind: "validation", Message: "invalid edit draft", Code: ExitValidation}
	}
	eligible, err := s.Revalidate(ctx, e)
	if err != nil {
		return &Error{Kind: "validation", Message: "edit revalidation failed", Code: ExitValidation}
	}
	encoded, _ := json.Marshal(eligible)
	if err = atomicCreate(*out, encoded); err != nil {
		return &Error{Kind: "validation", Message: "write attested edit failed", Code: ExitValidation}
	}
	o.json = *jsonFlag
	return o.success(map[string]any{"status": "revalidated", "edit_digest": e.Digest, "output": *out}, e.Digest)
}
func runPlanPublish(ctx context.Context, args []string, o output, s PlanEditService) error {
	fs := newFlags("plan publish", o.streams.Err)
	in := fs.String("attested", "", "attested edit")
	out := fs.String("output", "", "signed artifact output")
	jsonFlag := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if s == nil || *in == "" || *out == "" {
		return usageError(errors.New("configured plan editing plus --attested and --output required"))
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		return &Error{Kind: "validation", Message: "read attested edit failed", Code: ExitValidation}
	}
	var e planedit.Eligible
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&e); err != nil || e.Edit.Validate() != nil {
		return &Error{Kind: "validation", Message: "invalid attested edit", Code: ExitValidation}
	}
	a, err := s.Publish(ctx, e)
	if err != nil {
		return &Error{Kind: "validation", Message: "publish edited artifact failed", Code: ExitValidation}
	}
	encoded, err := a.MarshalCanonical()
	if err != nil {
		return &Error{Kind: "validation", Message: "published artifact invalid", Code: ExitValidation}
	}
	if err = atomicCreate(*out, encoded); err != nil {
		return &Error{Kind: "validation", Message: "write published artifact failed", Code: ExitValidation}
	}
	o.json = *jsonFlag
	return o.success(map[string]any{"status": "published", "artifact_digest": a.Digest, "output": *out}, a.Digest)
}
func atomicCreate(path string, data []byte) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = unix.Close(fd)
		if !ok {
			_ = unix.Unlink(path)
		}
	}()
	for len(data) > 0 {
		n, e := unix.Write(fd, data)
		if e != nil {
			return e
		}
		data = data[n:]
	}
	if err = unix.Fsync(fd); err != nil {
		return err
	}
	ok = true
	return nil
}
