package cli

import (
	"autosql/pkg/migrate"
	"autosql/pkg/plan"
	"context"
	"errors"
	"fmt"
	"strings"
)

type generateResult struct {
	Status, Version, File, ManifestDigest, PlanDigest string
	Changes                                           int
}

func runMigrateGenerate(ctx context.Context, args []string, o output, reader ReadPlanService) error {
	fs := newFlags("migrate generate", o.streams.Err)
	dir := fs.String("dir", "", "migration directory")
	from := fs.String("from", "", "current source")
	to := fs.String("to", "", "desired source")
	version := fs.String("version", "", "version")
	label := fs.String("label", "", "label")
	format := fs.String("format", "sql", "format")
	hints := fs.String("rename-hints", "", "rename hints")
	jf := fs.Bool("json", false, "JSON")
	if e := fs.Parse(args); e != nil {
		return usageError(e)
	}
	if reader == nil || *dir == "" || *from == "" || *to == "" || *version == "" || *label == "" || *format != "sql" || fs.NArg() != 0 {
		return usageError(errors.New("--dir, --from, --to, --version, --label and --format=sql required"))
	}
	if strings.ToLower(*label) != *label || strings.IndexFunc(*label, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') }) >= 0 {
		return usageError(errors.New("label must be canonical lowercase ASCII"))
	}
	if _, e := migrate.ParseVersion(*version); e != nil {
		return &Error{Kind: "validation", Message: "invalid migration version", Code: ExitValidation}
	}
	snap, e := migrate.LoadSnapshot(*dir)
	if e != nil {
		return &Error{Kind: "validation", Message: "verify migration directory failed", Code: ExitValidation, Cause: e}
	}
	if len(snap.Manifest.Entries) > 0 && snap.Manifest.Entries[len(snap.Manifest.Entries)-1].NonLinear {
		return &Error{Kind: "validation", Message: "generation requires a linear manifest head", Code: ExitValidation}
	}
	current, e := reader.Load(ctx, LoadRequest{Spec: *from})
	if e != nil {
		return &Error{Kind: "validation", Message: "load replayed schema failed", Code: ExitValidation}
	}
	desired, e := reader.Load(ctx, LoadRequest{Spec: *to})
	if e != nil {
		return &Error{Kind: "validation", Message: "load desired schema failed", Code: ExitValidation}
	}
	p, e := reader.Plan(ctx, current, desired)
	if e != nil {
		return &Error{Kind: "validation", Message: "plan migration failed", Code: ExitValidation}
	}
	o.json = *jf
	if len(p.Changes.Changes) == 0 {
		return o.success(generateResult{Status: "no_op", ManifestDigest: snap.Manifest.Digest, PlanDigest: p.Digest}, "no migration generated")
	}
	var ss []string
	for _, s := range p.Steps {
		if s.Kind == plan.StepExecutable {
			ss = append(ss, strings.TrimSpace(s.SQL))
		}
	}
	sql := fmt.Sprintf("-- autosql:transaction=auto\n-- autosql:plan-digest=%s\n-- autosql:check-digest=%s\n-- autosql:bundle-digest=%s\n-- autosql:check-bundle-digest=%s\n-- rename-hints: %s\n%s\n", p.Digest, p.Digest, p.Digest, p.Digest, *hints, strings.Join(ss, ";\n"))
	name := fmt.Sprintf("V%s__%s.sql", *version, *label)
	files := make([]migrate.File, 0, len(snap.Manifest.Entries)+1)
	for _, x := range snap.Manifest.Entries {
		files = append(files, migrate.File{Name: x.File, SQL: append([]byte(nil), snap.Files[x.File]...), Parents: append([]string(nil), x.Parents...), NonLinear: x.NonLinear})
	}
	files = append(files, migrate.File{Name: name, SQL: []byte(sql)})
	man, e := migrate.Update(*dir, migrate.UpdateRequest{Files: files, ManifestVersion: migrate.ManifestVersion, ExpectedManifestDigest: snap.Manifest.Digest})
	if e != nil {
		return &Error{Kind: "validation", Message: "publish migration generation failed", Code: ExitValidation, Cause: e}
	}
	return o.success(generateResult{Status: "generated", Version: *version, File: name, ManifestDigest: man.Digest, PlanDigest: p.Digest, Changes: len(p.Changes.Changes)}, name)
}
