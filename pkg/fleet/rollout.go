package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"autosql/pkg/artifact"
)

var (
	ErrInvalidRollout = errors.New("invalid fleet rollout plan")
	ErrGroupCycle     = errors.New("fleet rollout group dependency cycle")
	ErrOverlap        = errors.New("fleet rollout target belongs to multiple groups")
)

type RolloutRequest struct {
	Artifact artifact.VerifiedArtifact
	Snapshot Snapshot
	Groups   []Group
}

type Batch struct {
	Targets []Target `json:"targets"`
	Canary  bool     `json:"canary"`
}

type ExecutionGroup struct {
	Name        string        `json:"name"`
	DependsOn   []string      `json:"depends_on,omitempty"`
	Targets     []Target      `json:"targets"`
	Batches     []Batch       `json:"batches"`
	CanarySize  int           `json:"canary_size,omitempty"`
	MaxParallel int           `json:"max_parallel"`
	BatchSize   int           `json:"batch_size"`
	Delay       time.Duration `json:"delay,omitempty"`
	FailPolicy  FailPolicy    `json:"fail_policy"`
}

func BuildRollout(ctx context.Context, req RolloutRequest) (RolloutPlan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := req.Artifact.Payload(); err != nil {
		return RolloutPlan{}, fmt.Errorf("%w: artifact: %v", ErrInvalidRollout, err)
	}
	if err := req.Snapshot.Validate(); err != nil {
		return RolloutPlan{}, fmt.Errorf("%w: snapshot: %v", ErrInvalidRollout, err)
	}
	if len(req.Groups) == 0 {
		return RolloutPlan{}, fmt.Errorf("%w: no groups", ErrInvalidRollout)
	}
	byName := make(map[string]Group, len(req.Groups))
	for _, g := range req.Groups {
		if g.Name == "" || byName[g.Name].Name != "" || g.MaxParallel <= 0 || g.BatchSize <= 0 || g.CanarySize < 0 || g.Delay < 0 || (g.FailPolicy != FailFast && g.FailPolicy != Continue) {
			return RolloutPlan{}, ErrInvalidRollout
		}
		if err := g.Filter.Validate(); err != nil {
			return RolloutPlan{}, fmt.Errorf("%w: filter: %v", ErrInvalidRollout, err)
		}
		seen := map[string]bool{}
		for _, dep := range g.DependsOn {
			if dep == "" || dep == g.Name || seen[dep] {
				return RolloutPlan{}, ErrInvalidRollout
			}
			seen[dep] = true
		}
		byName[g.Name] = g
	}
	if err := validateDependencies(byName); err != nil {
		return RolloutPlan{}, err
	}
	assigned := map[string]string{}
	executionGroups := make([]ExecutionGroup, 0, len(req.Groups))
	groups := make([]Group, 0, len(req.Groups))
	for _, g := range req.Groups {
		targets, err := req.Snapshot.Select(g.Filter)
		if err != nil {
			return RolloutPlan{}, err
		}
		if g.CanarySize > len(targets) {
			return RolloutPlan{}, fmt.Errorf("%w: group %s has %d targets but requires %d canary targets", ErrInvalidRollout, g.Name, len(targets), g.CanarySize)
		}
		if g.CanarySize > 0 && len(targets) == 0 {
			return RolloutPlan{}, fmt.Errorf("%w: group %s empty required canary", ErrInvalidRollout, g.Name)
		}
		for _, t := range targets {
			if prior, ok := assigned[t.ID]; ok {
				return RolloutPlan{}, fmt.Errorf("%w: %s in %s and %s", ErrOverlap, t.ID, prior, g.Name)
			}
			assigned[t.ID] = g.Name
		}
		batches := makeBatches(targets, g.CanarySize, g.BatchSize)
		deps := append([]string(nil), g.DependsOn...)
		sort.Strings(deps)
		executionGroups = append(executionGroups, ExecutionGroup{Name: g.Name, DependsOn: deps, Targets: targets, Batches: batches, CanarySize: g.CanarySize, MaxParallel: g.MaxParallel, BatchSize: g.BatchSize, Delay: g.Delay, FailPolicy: g.FailPolicy})
		ids := make([]string, len(targets))
		for i := range targets {
			ids[i] = targets[i].ID
		}
		g.TargetIDs = ids
		g.ContinueOnFailure = g.FailPolicy == Continue
		groups = append(groups, g)
	}
	orderGroups(executionGroups, byName)
	return RolloutPlan{ID: req.Artifact.Digest() + "@" + req.Snapshot.ID, ArtifactDigest: req.Artifact.Digest(), TargetSnapshotDigest: req.Snapshot.ID, SnapshotID: req.Snapshot.ID, Groups: groups, ExecutionGroups: executionGroups}, nil
}

func validateDependencies(groups map[string]Group) error {
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(name string) error {
		if state[name] == 1 {
			return ErrGroupCycle
		}
		if state[name] == 2 {
			return nil
		}
		g, ok := groups[name]
		if !ok {
			return fmt.Errorf("%w: unknown dependency %s", ErrInvalidRollout, name)
		}
		state[name] = 1
		for _, dep := range g.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[name] = 2
		return nil
	}
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := visit(n); err != nil {
			return err
		}
	}
	return nil
}

func makeBatches(targets []Target, canary, size int) []Batch {
	out := make([]Batch, 0)
	if canary > 0 {
		out = append(out, Batch{Targets: cloneTargets(targets[:canary]), Canary: true})
		targets = targets[canary:]
	}
	for len(targets) > 0 {
		n := size
		if n > len(targets) {
			n = len(targets)
		}
		out = append(out, Batch{Targets: cloneTargets(targets[:n])})
		targets = targets[n:]
	}
	return out
}

func cloneTargets(in []Target) []Target {
	out := make([]Target, len(in))
	for i, t := range in {
		out[i] = cloneTarget(t)
	}
	return out
}

func orderGroups(groups []ExecutionGroup, all map[string]Group) {
	// BuildRollout retains declaration-independent deterministic ordering by
	// sorting groups in dependency topological order; ties use group name.
	byName := make(map[string]ExecutionGroup, len(groups))
	for _, g := range groups {
		byName[g.Name] = g
	}
	out := make([]ExecutionGroup, 0, len(groups))
	seen := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for _, dep := range byName[name].DependsOn {
			visit(dep)
		}
		out = append(out, byName[name])
	}
	names := make([]string, 0, len(groups))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		visit(n)
	}
	copy(groups, out)
}

// DryRun is a stable, JSON-ready execution preview. It includes exact masked
// target IDs, dependency order, batch/canary boundaries, and concurrency.
type DryRun struct {
	ArtifactDigest string           `json:"artifact_digest"`
	SnapshotID     string           `json:"snapshot_id"`
	Groups         []ExecutionGroup `json:"groups"`
}

func (p RolloutPlan) DryRun() DryRun {
	return DryRun{ArtifactDigest: p.ArtifactDigest, SnapshotID: p.SnapshotID, Groups: append([]ExecutionGroup(nil), p.ExecutionGroups...)}
}
