package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

type ApplyFunc func(context.Context, Target, string) error
type Transient interface{ Transient() bool }

func IsTransient(err error) bool { var t Transient; return errors.As(err, &t) && t.Transient() }

type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}
type Executor struct {
	Store *Store
	Apply ApplyFunc
	Retry RetryPolicy
	Now   func() time.Time
}

func (e *Executor) Execute(ctx context.Context, rolloutID string, p RolloutPlan, targets []Target) (RolloutState, error) {
	if e == nil || e.Store == nil || e.Apply == nil {
		return RolloutState{}, ErrInvalidRollout
	}
	if err := p.Validate(targets); err != nil {
		return RolloutState{}, err
	}
	if e.Retry.MaxAttempts < 1 {
		e.Retry.MaxAttempts = 1
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	r, err := e.Store.Get(rolloutID)
	if err != nil {
		r, err = e.Store.Create(rolloutID, p, targets, now)
	}
	if err != nil {
		return RolloutState{}, err
	}
	if r.ArtifactDigest != p.ArtifactDigest {
		return RolloutState{}, ErrConflict
	}
	byID := map[string]Target{}
	for _, t := range targets {
		byID[t.ID] = t
	}
	e.Store.setRollout(rolloutID, "running", now)
	groups := p.ExecutionGroups
	if len(groups) == 0 {
		return RolloutState{}, ErrInvalidRollout
	}
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			e.Store.cancelPending(rolloutID, err.Error(), now)
			return e.Store.Get(rolloutID)
		}
		for _, dep := range g.DependsOn {
			current, _ := e.Store.Get(rolloutID)
			if !groupSucceeded(current, groups, dep) {
				e.Store.setRollout(rolloutID, "paused", now)
				return e.Store.Get(rolloutID)
			}
		}
		for _, batch := range g.Batches {
			ids := make([]string, 0, len(batch.Targets))
			for _, t := range batch.Targets {
				ids = append(ids, t.ID)
			}
			limit := g.MaxParallel
			if limit < 1 {
				limit = 1
			}
			sem := make(chan struct{}, limit)
			var wg sync.WaitGroup
			for _, id := range ids {
				id := id
				st, _ := e.Store.target(rolloutID, id)
				if st.State == Succeeded || st.State == Skipped {
					continue
				}
				t, ok := byID[id]
				if !ok {
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					_ = e.runTarget(ctx, rolloutID, t, p.ArtifactDigest)
				}()
			}
			wg.Wait()
			if ctx.Err() != nil {
				e.Store.cancelPending(rolloutID, ctx.Err().Error(), now)
				return e.Store.Get(rolloutID)
			}
			if g.FailPolicy == FailFast && e.Store.anyFailed(rolloutID, ids) {
				e.Store.setRollout(rolloutID, "failed", now)
				return e.Store.Get(rolloutID)
			}
			if g.Delay > 0 {
				timer := time.NewTimer(time.Duration(g.Delay))
				select {
				case <-ctx.Done():
					timer.Stop()
					e.Store.cancelPending(rolloutID, ctx.Err().Error(), now)
					return e.Store.Get(rolloutID)
				case <-timer.C:
				}
			}
		}
	}
	e.Store.setRollout(rolloutID, "succeeded", now)
	return e.Store.Get(rolloutID)
}
func groupSucceeded(r RolloutState, gs []ExecutionGroup, name string) bool {
	for _, g := range gs {
		if g.Name == name {
			for _, b := range g.Batches {
				for _, t := range b.Targets {
					if x, ok := r.Targets[t.ID]; !ok || x.State != Succeeded {
						return false
					}
				}
			}
			return true
		}
	}
	return false
}
func (e *Executor) runTarget(ctx context.Context, rolloutID string, t Target, artifact string) error {
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	if err := e.Store.claim(rolloutID, t.ID, artifact, now); err != nil {
		return err
	}
	for attempt := 1; attempt <= e.Retry.MaxAttempts; attempt++ {
		e.Store.attempt(rolloutID, t.ID, attempt, now)
		err := e.Apply(ctx, t, artifact)
		if err == nil {
			if ctx.Err() != nil {
				e.Store.finish(rolloutID, t.ID, Canceled, ctx.Err().Error(), now)
				return ctx.Err()
			}
			e.Store.finish(rolloutID, t.ID, Succeeded, "", now)
			return nil
		}
		if !IsTransient(err) || attempt == e.Retry.MaxAttempts {
			e.Store.finish(rolloutID, t.ID, Failed, redactError(err.Error()), now)
			return err
		}
		if e.Retry.Backoff > 0 {
			timer := time.NewTimer(e.Retry.Backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				e.Store.finish(rolloutID, t.ID, Canceled, ctx.Err().Error(), now)
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil
}
func redactError(s string) string {
	for _, x := range []string{"password=", "token=", "secret=", "postgres://", "postgresql://"} {
		if strings.Contains(strings.ToLower(s), x) {
			return "redacted deployment error"
		}
	}
	return s
}
func (s *Store) target(id, tid string) (TargetResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rollouts[id]
	if !ok {
		return TargetResult{}, ErrNotFound
	}
	x, ok := r.Targets[tid]
	if !ok {
		return TargetResult{}, ErrNotFound
	}
	return x, nil
}
func (s *Store) setRollout(id, state string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rollouts[id]; ok {
		r.State = state
		r.UpdatedAt = at.UTC()
		s.rollouts[id] = r
	}
}
func (s *Store) claim(id, tid, artifact string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rollouts[id]
	if !ok {
		return ErrNotFound
	}
	x, ok := r.Targets[tid]
	if !ok {
		return ErrNotFound
	}
	if x.State == Succeeded {
		return nil
	}
	key := tid + "\x00" + artifact
	if owner, active := s.active[key]; active && owner != id {
		return ErrConflict
	}
	s.active[key] = id
	x.State = Running
	x.UpdatedAt = at.UTC()
	r.Targets[tid] = x
	r.Events = append(r.Events, Event{At: at.UTC(), Type: "target_started", RolloutID: id, TargetID: tid, ArtifactDigest: artifact})
	s.rollouts[id] = r
	return nil
}
func (s *Store) attempt(id, tid string, n int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rollouts[id]; ok {
		if x, ok := r.Targets[tid]; ok {
			x.Attempts = n
			x.UpdatedAt = at.UTC()
			r.Targets[tid] = x
			s.rollouts[id] = r
		}
	}
}
func (s *Store) finish(id, tid string, state TargetState, msg string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rollouts[id]; ok {
		if x, ok := r.Targets[tid]; ok {
			x.State = state
			x.LastError = msg
			x.UpdatedAt = at.UTC()
			r.Targets[tid] = x
			delete(s.active, tid+"\x00"+r.ArtifactDigest)
			r.Events = append(r.Events, Event{At: at.UTC(), Type: "target_" + string(state), RolloutID: id, TargetID: tid, ArtifactDigest: r.ArtifactDigest, Reason: msg})
			s.rollouts[id] = r
		}
	}
}
func (s *Store) cancelPending(id, reason string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rollouts[id]; ok {
		for tid, x := range r.Targets {
			if x.State == Pending {
				x.State = Canceled
				x.LastError = reason
				x.UpdatedAt = at.UTC()
				r.Targets[tid] = x
			}
		}
		r.State = "canceled"
		r.UpdatedAt = at.UTC()
		r.Events = append(r.Events, Event{At: at.UTC(), Type: "rollout_canceled", RolloutID: id, Reason: reason})
		s.rollouts[id] = r
	}
}
func (s *Store) anyFailed(id string, ids []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rollouts[id]
	for _, tid := range ids {
		if r.Targets[tid].State == Failed {
			return true
		}
	}
	return false
}
