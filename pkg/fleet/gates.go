package fleet

import (
	"context"
	"errors"
	"strings"
	"time"
)

type Evidence struct {
	RolloutID, Stage, ArtifactDigest, TargetSnapshotDigest string
	Checks                                                 map[string]string
}
type Approval struct {
	RolloutID, Stage, ArtifactDigest, TargetSnapshotDigest, Approver, Reason string
	At                                                                       time.Time
	Emergency                                                                bool
}
type GatePolicy struct {
	RequiredChecks      []string
	AuthorizedApprovers map[string]bool
	AllowEmergency      bool
}

func (p GatePolicy) Evaluate(e Evidence, a *Approval) error {
	for _, k := range p.RequiredChecks {
		if e.Checks == nil || e.Checks[k] != "pass" {
			return errors.New("required health evidence missing: " + k)
		}
	}
	if a == nil || a.RolloutID != e.RolloutID || a.Stage != e.Stage || a.ArtifactDigest != e.ArtifactDigest || a.TargetSnapshotDigest != e.TargetSnapshotDigest || a.Approver == "" || a.At.IsZero() {
		return ErrGate
	}
	if a.Emergency && !p.AllowEmergency {
		return ErrPolicy
	}
	if !p.AuthorizedApprovers[a.Approver] && !a.Emergency {
		return ErrUnauthorized
	}
	if a.Emergency && strings.TrimSpace(a.Reason) == "" {
		return ErrGate
	}
	return nil
}

type GateAudit func(context.Context, Event) error

func (p GatePolicy) Authorize(ctx context.Context, e Evidence, a *Approval, audit GateAudit) error {
	if err := p.Evaluate(e, a); err != nil {
		return err
	}
	if audit != nil {
		return audit(ctx, Event{At: a.At.UTC(), Type: "gate_approved", RolloutID: a.RolloutID, ArtifactDigest: a.ArtifactDigest, Reason: a.Reason})
	}
	return nil
}

type HookPhase string

const (
	PreStage  HookPhase = "pre-stage"
	PerTarget HookPhase = "per-target"
	PostStage HookPhase = "post-stage"
)

type Hook struct {
	ID          string
	Phase       HookPhase
	Timeout     time.Duration
	Retries     int
	FailRollout bool
	Run         func(context.Context, HookInput) (HookOutput, error)
	Cleanup     func(context.Context) error
}
type HookInput struct {
	RolloutID, Stage, TargetID, ArtifactDigest string
	Secrets                                    map[string]string
}
type HookOutput struct {
	Message string
	Checks  map[string]string
}
type HookResult struct {
	ID       string
	Attempt  int
	Output   HookOutput
	Err      string
	TimedOut bool
}

func RunHook(ctx context.Context, h Hook, in HookInput) (HookResult, error) {
	if h.ID == "" || h.Run == nil || in.ArtifactDigest == "" || h.Timeout <= 0 {
		return HookResult{}, ErrInvalid
	}
	in.Secrets = cloneMap(in.Secrets)
	for k := range in.Secrets {
		if k == "" {
			return HookResult{}, ErrInvalid
		}
	}
	retries := h.Retries
	if retries < 0 {
		retries = 0
	}
	for attempt := 1; attempt <= retries+1; attempt++ {
		c, cancel := context.WithTimeout(ctx, h.Timeout)
		out, err := h.Run(c, in)
		timed := errors.Is(c.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil && !timed {
			out.Message = redact(out.Message, in.Secrets)
			return HookResult{ID: h.ID, Attempt: attempt, Output: out}, nil
		}
		if attempt == retries+1 {
			if h.Cleanup != nil {
				_ = h.Cleanup(context.WithoutCancel(ctx))
			}
			if timed {
				return HookResult{ID: h.ID, Attempt: attempt, TimedOut: true, Err: "hook timeout"}, context.DeadlineExceeded
			}
			if err == nil {
				err = context.DeadlineExceeded
			}
			return HookResult{ID: h.ID, Attempt: attempt, Err: redact(err.Error(), in.Secrets)}, err
		}
	}
	return HookResult{}, ErrInvalid
}

func redact(s string, secrets map[string]string) string {
	for _, v := range secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return s
}
