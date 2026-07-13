package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

type StatusFilter struct {
	RolloutID, Environment, TargetID, ArtifactDigest string
	States                                           []TargetState
}
type TargetStatus struct {
	RolloutID, TargetID, ArtifactDigest string
	State                               TargetState
	Attempts                            int
	LastError                           string
	UpdatedAt                           time.Time
	Drift                               bool
	Environment                         string
	CurrentArtifact, ExpectedArtifact   string
}

func (s *Store) Status(f StatusFilter) []TargetStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []TargetStatus{}
	for rid, r := range s.rollouts {
		if f.RolloutID != "" && rid != f.RolloutID || f.ArtifactDigest != "" && r.ArtifactDigest != f.ArtifactDigest {
			continue
		}
		for tid, x := range r.Targets {
			meta := r.TargetMeta[tid]
			if f.Environment != "" && meta.Environment != f.Environment {
				continue
			}
			if f.TargetID != "" && tid != f.TargetID || len(f.States) > 0 && !hasState(f.States, x.State) {
				continue
			}
			out = append(out, TargetStatus{RolloutID: rid, TargetID: tid, ArtifactDigest: r.ArtifactDigest, State: x.State, Attempts: x.Attempts, LastError: x.LastError, UpdatedAt: x.UpdatedAt, Drift: x.State == Drifted, Environment: meta.Environment, CurrentArtifact: meta.CurrentArtifact, ExpectedArtifact: meta.ExpectedArtifact})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RolloutID == out[j].RolloutID {
			return out[i].TargetID < out[j].TargetID
		}
		return out[i].RolloutID < out[j].RolloutID
	})
	return out
}
func hasState(xs []TargetState, x TargetState) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}
func (s *Store) StatusJSON(f StatusFilter) ([]byte, error) { return json.Marshal(s.Status(f)) }
func (s *Store) StatusHuman(f StatusFilter) string {
	rows := s.Status(f)
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.RolloutID + " " + r.TargetID + " " + string(r.State) + "\n")
	}
	return b.String()
}

type Audit func(context.Context, Event) error
type RecoveryPolicy struct{ AllowSkip, AllowRollback bool }
type Recovery struct {
	Store  *Store
	Policy RecoveryPolicy
	Audit  Audit
	Now    func() time.Time
}

func (r *Recovery) Preview(f StatusFilter) []TargetStatus {
	if r == nil || r.Store == nil {
		return nil
	}
	return r.Store.Status(f)
}
func (r *Recovery) Retry(ctx context.Context, f StatusFilter, reason string) ([]TargetStatus, error) {
	return r.mutate(ctx, f, reason, "retry", func(x *TargetResult) error {
		if x.State != Failed && x.State != Canceled {
			return errors.New("target is not retryable")
		}
		x.State = Pending
		return nil
	})
}
func (r *Recovery) Skip(ctx context.Context, f StatusFilter, reason string) ([]TargetStatus, error) {
	if r == nil || !r.Policy.AllowSkip {
		return nil, ErrPolicy
	}
	return r.mutate(ctx, f, reason, "skip", func(x *TargetResult) error {
		if x.State == Succeeded {
			return errors.New("target already succeeded")
		}
		x.State = Skipped
		return nil
	})
}
func (r *Recovery) Rollback(ctx context.Context, f StatusFilter, reason string, down func(context.Context, TargetStatus) error) ([]TargetStatus, error) {
	if r == nil || !r.Policy.AllowRollback || down == nil {
		return nil, ErrPolicy
	}
	rows := r.Preview(f)
	if strings.TrimSpace(reason) == "" {
		return nil, ErrUnauthorized
	}
	for _, x := range rows {
		if err := down(ctx, x); err != nil {
			return nil, err
		}
	}
	for _, x := range rows {
		if r.Audit != nil {
			if err := r.Audit(ctx, Event{At: r.now(), Type: "rollback_requested", RolloutID: x.RolloutID, TargetID: x.TargetID, ArtifactDigest: x.ArtifactDigest, Reason: reason}); err != nil {
				return nil, err
			}
		}
	}
	return rows, nil
}
func (r *Recovery) mutate(ctx context.Context, f StatusFilter, reason, kind string, fn func(*TargetResult) error) ([]TargetStatus, error) {
	if r == nil || r.Store == nil || strings.TrimSpace(reason) == "" {
		return nil, ErrUnauthorized
	}
	rows := r.Preview(f)
	for _, x := range rows {
		if err := r.Store.mutateTarget(x.RolloutID, x.TargetID, fn, r.now()); err != nil {
			return nil, err
		}
		if r.Audit != nil {
			if err := r.Audit(ctx, Event{At: r.now(), Type: kind + "_requested", RolloutID: x.RolloutID, TargetID: x.TargetID, ArtifactDigest: x.ArtifactDigest, Reason: reason}); err != nil {
				return nil, err
			}
		}
	}
	return r.Preview(f), nil
}
func (r *Recovery) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *Store) mutateTarget(rid, tid string, fn func(*TargetResult) error, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rollouts[rid]
	if !ok {
		return ErrNotFound
	}
	x, ok := r.Targets[tid]
	if !ok {
		return ErrNotFound
	}
	if err := fn(&x); err != nil {
		return err
	}
	x.UpdatedAt = at
	r.Targets[tid] = x
	r.UpdatedAt = at
	s.rollouts[rid] = r
	return nil
}
