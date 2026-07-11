// Package approval enforces trusted authorization immediately before a
// migration is applied. GuardedApply is fail-closed: it durably records a
// tamper-evident decision and never invokes mutation after a gate failure.
package approval

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Risk int

const (
	RiskLow Risk = iota
	RiskMedium
	RiskHigh
	RiskCritical
)

type Plan struct {
	Digest, Environment, Author string
	Risk                        Risk
	ExpiresAt                   time.Time
}

// Approval carries identity and binding data but no roles. Proof is opaque to
// this package and must be verified by the configured IdentityAuthority.
type Approval struct {
	PlanDigest, Environment, Approver string
	ApprovedAt, ExpiresAt             time.Time
	Proof                             string
}

type Identity struct {
	ID                 string
	Roles              []string
	EmergencyAuthority bool
}

type VerifiedApproval struct {
	Identity                Identity
	PlanDigest, Environment string
	ApprovedAt, ExpiresAt   time.Time
}

// IdentityAuthority is the trusted boundary for identities, roles, and proof.
// VerifyApproval must return claims authenticated by Proof, including plan,
// environment, and timestamps; Gate never trusts those transport fields alone.
type IdentityAuthority interface {
	ResolveActor(context.Context, string) (Identity, error)
	VerifyApproval(context.Context, Approval) (VerifiedApproval, error)
}

type Requirement struct {
	MinimumRisk   Risk
	ApproverCount int
	Roles         []string
}

type EnvironmentPolicy struct {
	Allowed      bool
	Requirements []Requirement
}

type Policy struct{ Environments map[string]EnvironmentPolicy }

type EmergencyOverride struct{ Identity, Reason string }

type Request struct {
	Plan        Plan
	Approvals   []Approval
	Override    *EmergencyOverride
	RequestedBy string
}

var (
	ErrDenied        = errors.New("apply approval denied")
	ErrAudit         = errors.New("approval audit failed")
	ErrAuditConflict = errors.New("audit chain conflict")
)

type Decision struct {
	Allowed   bool
	Emergency bool
	Approvers []string
	Reason    string
	expiresAt map[string]time.Time
}

type Event struct {
	At          time.Time `json:"at"`
	Type        string    `json:"type"`
	PlanDigest  string    `json:"plan_digest"`
	Environment string    `json:"environment"`
	Actor       string    `json:"actor"`
	Reason      string    `json:"reason,omitempty"`
	Approvers   []string  `json:"approvers,omitempty"`
	Emergency   bool      `json:"emergency"`
	Allowed     bool      `json:"allowed"`
}

type AuditRecord struct {
	Sequence     uint64 `json:"sequence"`
	PreviousHash string `json:"previous_hash,omitempty"`
	Hash         string `json:"hash"`
	Event        Event  `json:"event"`
}

// DurableSink atomically appends a record only when expectedPreviousHash is
// still the chain head. Tail must validate the complete chain. Append success
// means the record is durable, not merely queued.
type DurableSink interface {
	Tail(context.Context) (*AuditRecord, error)
	AppendDurable(context.Context, string, AuditRecord) error
}

type AuditTrail interface {
	AppendDurable(context.Context, Event) (AuditRecord, error)
}

// Chain turns a durable append sink into a hash-linked, tamper-evident trail.
type Chain struct {
	Sink DurableSink
	mu   sync.Mutex
}

func (c *Chain) AppendDurable(ctx context.Context, event Event) (AuditRecord, error) {
	if c == nil || c.Sink == nil {
		return AuditRecord{}, fmt.Errorf("%w: durable sink is required", ErrAudit)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tail, err := c.Sink.Tail(ctx)
	if err != nil {
		return AuditRecord{}, fmt.Errorf("%w: read tail: %v", ErrAudit, err)
	}
	var sequence uint64 = 1
	var previous string
	if tail != nil {
		if !verifyHash(*tail) {
			return AuditRecord{}, fmt.Errorf("%w: invalid tail hash", ErrAudit)
		}
		sequence, previous = tail.Sequence+1, tail.Hash
	}
	event.At = event.At.UTC()
	event.Approvers = append([]string(nil), event.Approvers...)
	record := AuditRecord{Sequence: sequence, PreviousHash: previous, Event: event}
	record.Hash = hashRecord(record)
	if err := c.Sink.AppendDurable(ctx, previous, record); err != nil {
		return AuditRecord{}, fmt.Errorf("%w: append: %v", ErrAudit, err)
	}
	return record, nil
}

func hashRecord(r AuditRecord) string {
	payload := struct {
		Sequence     uint64 `json:"sequence"`
		PreviousHash string `json:"previous_hash,omitempty"`
		Event        Event  `json:"event"`
	}{r.Sequence, r.PreviousHash, r.Event}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func verifyHash(r AuditRecord) bool { return r.Hash != "" && r.Hash == hashRecord(r) }

// FileSink is a durable JSON-lines sink. It validates the complete chain before
// reads and appends, writes with O_APPEND, and fsyncs before reporting success.
type FileSink struct {
	Path string
}

var fileLocks sync.Map

func auditFileLock(path string) *sync.Mutex {
	clean := filepath.Clean(path)
	lock, _ := fileLocks.LoadOrStore(clean, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *FileSink) Tail(ctx context.Context) (*AuditRecord, error) {
	lock := auditFileLock(s.Path)
	lock.Lock()
	defer lock.Unlock()
	var tail *AuditRecord
	err := s.withOSLock(ctx, func() error {
		records, err := s.load(ctx)
		if err != nil || len(records) == 0 {
			return err
		}
		r := records[len(records)-1]
		tail = &r
		return nil
	})
	return tail, err
}

func (s *FileSink) AppendDurable(ctx context.Context, expected string, record AuditRecord) error {
	lock := auditFileLock(s.Path)
	lock.Lock()
	defer lock.Unlock()
	return s.withOSLock(ctx, func() error { return s.appendLocked(ctx, expected, record) })
}

func (s *FileSink) appendLocked(ctx context.Context, expected string, record AuditRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	records, err := s.load(ctx)
	if err != nil {
		return err
	}
	actual := ""
	if len(records) > 0 {
		actual = records[len(records)-1].Hash
	}
	if actual != expected {
		return ErrAuditConflict
	}
	if !verifyHash(record) || record.PreviousHash != expected || record.Sequence != uint64(len(records)+1) {
		return errors.New("invalid audit record")
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	if err = enc.Encode(record); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		if dir, openErr := os.Open(filepath.Dir(s.Path)); openErr == nil {
			err = dir.Sync()
			_ = dir.Close()
		} else {
			err = openErr
		}
	}
	return err
}

func (s *FileSink) withOSLock(ctx context.Context, fn func() error) error {
	lockFile, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	for {
		err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *FileSink) load(ctx context.Context) ([]AuditRecord, error) {
	f, err := os.Open(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var records []AuditRecord
	previous := ""
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var r AuditRecord
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return nil, err
		}
		if r.Sequence != uint64(len(records)+1) || r.PreviousHash != previous || !verifyHash(r) {
			return nil, errors.New("tampered audit chain")
		}
		records, previous = append(records, r), r.Hash
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return records, nil
}

type Gate struct {
	Policy    Policy
	Authority IdentityAuthority
	Audit     AuditTrail
	Now       func() time.Time
}

func (g Gate) Evaluate(ctx context.Context, req Request) (Decision, error) {
	return g.evaluateAt(ctx, req, g.now())
}

func (g Gate) evaluateAt(ctx context.Context, req Request, instant time.Time) (Decision, error) {
	deny := func(reason string) (Decision, error) {
		return Decision{Reason: reason}, fmt.Errorf("%w: %s", ErrDenied, reason)
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if strings.TrimSpace(req.Plan.Digest) == "" || strings.TrimSpace(req.Plan.Environment) == "" || strings.TrimSpace(req.Plan.Author) == "" || strings.TrimSpace(req.RequestedBy) == "" {
		return deny("plan digest, environment, author, and requester are required")
	}
	if req.Plan.Author == req.RequestedBy {
		return deny("plan author and apply requester must be different")
	}
	if req.Plan.Risk < RiskLow || req.Plan.Risk > RiskCritical {
		return deny("invalid plan risk")
	}
	if g.Authority == nil {
		return deny("trusted identity authority is required")
	}
	if author, err := g.Authority.ResolveActor(ctx, req.Plan.Author); err != nil || author.ID != req.Plan.Author {
		return deny("plan author identity is not trusted")
	}
	requester, err := g.Authority.ResolveActor(ctx, req.RequestedBy)
	if err != nil || requester.ID != req.RequestedBy {
		return deny("apply requester identity is not trusted")
	}
	ep, ok := g.Policy.Environments[req.Plan.Environment]
	if !ok || !ep.Allowed {
		return deny(fmt.Sprintf("environment %q is not allowed", req.Plan.Environment))
	}
	if !req.Plan.ExpiresAt.IsZero() && !req.Plan.ExpiresAt.After(instant) {
		return deny("plan has expired")
	}
	if req.Override != nil {
		if strings.TrimSpace(req.Override.Identity) == "" || strings.TrimSpace(req.Override.Reason) == "" {
			return deny("emergency identity and reason are required")
		}
		if req.Override.Identity != req.RequestedBy || !requester.EmergencyAuthority {
			return deny("requester is not authorized for emergency override")
		}
		return Decision{Allowed: true, Emergency: true, Reason: req.Override.Reason}, nil
	}
	type verifiedApproval struct {
		identity  Identity
		expiresAt time.Time
	}
	valid := map[string]verifiedApproval{}
	for _, a := range req.Approvals {
		if a.Approver == "" {
			continue
		}
		verified, err := g.Authority.VerifyApproval(ctx, a)
		identity := verified.Identity
		if err != nil || identity.ID != a.Approver || verified.PlanDigest != req.Plan.Digest || verified.Environment != req.Plan.Environment || verified.ApprovedAt.After(instant) || !verified.ExpiresAt.After(instant) || identity.ID == req.Plan.Author || identity.ID == req.RequestedBy {
			continue
		}
		valid[identity.ID] = verifiedApproval{identity: identity, expiresAt: verified.ExpiresAt}
	}
	eligible := map[string]bool{}
	expiresAt := map[string]time.Time{}
	for _, requirement := range ep.Requirements {
		if requirement.MinimumRisk < RiskLow || requirement.MinimumRisk > RiskCritical || requirement.ApproverCount < 0 {
			return deny("invalid approval requirement")
		}
		for _, role := range requirement.Roles {
			if strings.TrimSpace(role) == "" {
				return deny("approval roles cannot be empty")
			}
		}
		if req.Plan.Risk < requirement.MinimumRisk {
			continue
		}
		count := 0
		for _, approval := range valid {
			if hasAllRoles(approval.identity.Roles, requirement.Roles) {
				count++
				eligible[approval.identity.ID] = true
				expiresAt[approval.identity.ID] = approval.expiresAt
			}
		}
		if count < requirement.ApproverCount {
			return deny(fmt.Sprintf("requires %d eligible approvers with roles %v", requirement.ApproverCount, requirement.Roles))
		}
	}
	names := make([]string, 0, len(eligible))
	for name := range eligible {
		names = append(names, name)
	}
	sort.Strings(names)
	return Decision{Allowed: true, Approvers: names, expiresAt: expiresAt}, nil
}

func hasAllRoles(got, want []string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (g Gate) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

func approvalsFresh(req Request, decision Decision, at time.Time) bool {
	if !req.Plan.ExpiresAt.IsZero() && !req.Plan.ExpiresAt.After(at) {
		return false
	}
	if decision.Emergency {
		return true
	}
	for _, approver := range decision.Approvers {
		if expiry, ok := decision.expiresAt[approver]; !ok || !expiry.After(at) {
			return false
		}
	}
	return true
}

// GuardedApply persists the decision before mutation and then rechecks context,
// plan expiry, and every approval expiry at the final mutation boundary.
func (g Gate) GuardedApply(ctx context.Context, req Request, mutate func(context.Context) error) error {
	if mutate == nil {
		return fmt.Errorf("%w: apply mutation is required", ErrDenied)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	decision, decisionErr := g.evaluateAt(ctx, req, g.now())
	if g.Audit == nil {
		return fmt.Errorf("%w: durable audit trail is required", ErrAudit)
	}
	actor := req.RequestedBy
	if decision.Emergency && req.Override != nil {
		actor = req.Override.Identity
	}
	event := Event{At: g.now(), Type: "apply_denied", PlanDigest: req.Plan.Digest, Environment: req.Plan.Environment, Actor: actor, Reason: decision.Reason, Approvers: append([]string(nil), decision.Approvers...), Emergency: decision.Emergency, Allowed: decision.Allowed}
	if decisionErr == nil {
		event.Type = "apply_authorized"
	}
	if _, err := g.Audit.AppendDurable(ctx, event); err != nil {
		return fmt.Errorf("%w: %v", ErrAudit, err)
	}
	if decisionErr != nil {
		return decisionErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !approvalsFresh(req, decision, g.now()) {
		denied := event
		denied.At, denied.Type, denied.Allowed, denied.Reason = g.now(), "apply_denied", false, "authorization expired before mutation"
		if _, err := g.Audit.AppendDurable(ctx, denied); err != nil {
			return fmt.Errorf("%w: persist expiry denial: %v", ErrAudit, err)
		}
		return fmt.Errorf("%w: authorization expired before mutation", ErrDenied)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return mutate(ctx)
}
