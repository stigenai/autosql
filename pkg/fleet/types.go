package fleet

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type FailPolicy string

const (
	FailFast FailPolicy = "fail_fast"
	Continue FailPolicy = "continue"
)

// Group is the normalized rollout group. Filter is used when constructing a
// plan; TargetIDs are filled into the durable plan for deterministic replay.
type Group struct {
	Name              string        `json:"name"`
	TargetIDs         []string      `json:"target_ids,omitempty"`
	Filter            TargetFilter  `json:"filter,omitempty"`
	DependsOn         []string      `json:"depends_on,omitempty"`
	CanarySize        int           `json:"canary_size,omitempty"`
	MaxParallel       int           `json:"max_parallel"`
	BatchSize         int           `json:"batch_size"`
	Delay             time.Duration `json:"delay,omitempty"`
	ContinueOnFailure bool          `json:"continue_on_failure,omitempty"`
	FailPolicy        FailPolicy    `json:"fail_policy,omitempty"`
}

type RolloutPlan struct {
	ID                   string           `json:"id"`
	ArtifactDigest       string           `json:"artifact_digest"`
	TargetSnapshotDigest string           `json:"target_snapshot_digest"`
	SnapshotID           string           `json:"snapshot_id"`
	Groups               []Group          `json:"groups"`
	ExecutionGroups      []ExecutionGroup `json:"execution_groups,omitempty"`
}

func (p RolloutPlan) Validate(targets []Target) error {
	if p.ID == "" || !digestPattern.MatchString(p.ArtifactDigest) || !digestPattern.MatchString(p.TargetSnapshotDigest) || p.SnapshotID == "" || len(p.Groups) == 0 {
		return ErrInvalidRollout
	}
	known := map[string]bool{}
	for _, t := range targets {
		if err := t.Validate(); err != nil || known[t.ID] {
			return ErrInvalidRollout
		}
		known[t.ID] = true
	}
	groups := map[string]Group{}
	assigned := map[string]bool{}
	for _, g := range p.Groups {
		if strings.TrimSpace(g.Name) == "" || groups[g.Name].Name != "" || g.MaxParallel <= 0 || g.BatchSize <= 0 || g.CanarySize < 0 || g.CanarySize > len(g.TargetIDs) {
			return ErrInvalidRollout
		}
		if g.FailPolicy != "" && g.FailPolicy != FailFast && g.FailPolicy != Continue {
			return ErrInvalidRollout
		}
		for _, id := range g.TargetIDs {
			if !known[id] || assigned[id] {
				return ErrInvalidRollout
			}
			assigned[id] = true
		}
		groups[g.Name] = g
	}
	if err := validateDependencies(groups); err != nil {
		return err
	}
	return nil
}

func (p RolloutPlan) GroupNames() []string {
	groups := map[string]Group{}
	for _, g := range p.Groups {
		groups[g.Name] = g
	}
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		for _, d := range groups[n].DependsOn {
			visit(d)
		}
		out = append(out, n)
	}
	for _, n := range names {
		visit(n)
	}
	return out
}

func (p RolloutPlan) String() string {
	return fmt.Sprintf("rollout %s artifact=%s snapshot=%s groups=%d", p.ID, p.ArtifactDigest, p.TargetSnapshotDigest, len(p.Groups))
}
