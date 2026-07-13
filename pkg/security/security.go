// Package security models database principals, permissions, row-level
// policies, and their safe declarative reconciliation.
package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"autosql/pkg/secret"
)

var (
	ErrInvalidState = errors.New("invalid security state")
	ErrExternal     = errors.New("external principal is protected")
	ErrLockout      = errors.New("security change would remove executing access")
	ErrPolicy       = errors.New("security policy violation")
	ErrApply        = errors.New("security apply failed")
)

type PrincipalKind string

const (
	Role PrincipalKind = "role"
	User PrincipalKind = "user"
)

// Principal deliberately contains no password, token, or authentication
// material. AuthRef is an opaque secret reference resolved only at runtime.
type Principal struct {
	Name            string            `json:"name"`
	Kind            PrincipalKind     `json:"kind"`
	Managed         bool              `json:"managed"`
	External        bool              `json:"external"`
	Login           bool              `json:"login,omitempty"`
	Inherit         bool              `json:"inherit,omitempty"`
	Superuser       bool              `json:"superuser,omitempty"`
	CreateRole      bool              `json:"create_role,omitempty"`
	CreateDatabase  bool              `json:"create_database,omitempty"`
	BypassRLS       bool              `json:"bypass_rls,omitempty"`
	ConnectionLimit int               `json:"connection_limit,omitempty"`
	ValidUntil      string            `json:"valid_until,omitempty"`
	AuthRef         secret.Reference  `json:"auth_ref,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

type Membership struct {
	Member string `json:"member"`
	Parent string `json:"parent"`
	Admin  bool   `json:"admin,omitempty"`
}

type Grant struct {
	Object    string `json:"object"`
	Grantee   string `json:"grantee"`
	Grantor   string `json:"grantor"`
	Privilege string `json:"privilege"`
	Grantable bool   `json:"grantable,omitempty"`
}

type DefaultPrivilege struct {
	Owner      string `json:"owner"`
	ObjectType string `json:"object_type"`
	Schema     string `json:"schema,omitempty"`
	Grantee    string `json:"grantee"`
	Privilege  string `json:"privilege"`
}

type RowPolicy struct {
	Name       string   `json:"name"`
	Table      string   `json:"table"`
	Command    string   `json:"command"`
	Roles      []string `json:"roles,omitempty"`
	Using      string   `json:"using,omitempty"`
	Check      string   `json:"check,omitempty"`
	Permissive bool     `json:"permissive,omitempty"`
}

type State struct {
	Principals        []Principal        `json:"principals"`
	Memberships       []Membership       `json:"memberships,omitempty"`
	Grants            []Grant            `json:"grants,omitempty"`
	DefaultPrivileges []DefaultPrivilege `json:"default_privileges,omitempty"`
	Policies          []RowPolicy        `json:"policies,omitempty"`
}

func (s State) Validate() error {
	seen := map[string]bool{}
	for _, p := range s.Principals {
		if strings.TrimSpace(p.Name) == "" || (p.Kind != Role && p.Kind != User) {
			return fmt.Errorf("%w: principal name and kind are required", ErrInvalidState)
		}
		if seen[p.Name] {
			return fmt.Errorf("%w: duplicate principal %q", ErrInvalidState, p.Name)
		}
		seen[p.Name] = true
		if p.External && p.Managed {
			return fmt.Errorf("%w: principal %q cannot be both managed and external", ErrInvalidState, p.Name)
		}
		if p.AuthRef != "" {
			if err := p.AuthRef.Validate(); err != nil {
				return fmt.Errorf("%w: principal %q auth reference: %v", ErrInvalidState, p.Name, err)
			}
		}
	}
	for _, m := range s.Memberships {
		if m.Member == "" || m.Parent == "" || m.Member == m.Parent || !seen[m.Member] || !seen[m.Parent] {
			return fmt.Errorf("%w: invalid membership %q -> %q", ErrInvalidState, m.Member, m.Parent)
		}
	}
	for _, g := range s.Grants {
		if g.Object == "" || g.Grantee == "" || g.Grantor == "" || g.Privilege == "" || (g.Grantee != "PUBLIC" && !seen[g.Grantee]) || (g.Grantor != "PUBLIC" && !seen[g.Grantor]) {
			return fmt.Errorf("%w: invalid grant on %q", ErrInvalidState, g.Object)
		}
	}
	for _, p := range s.Policies {
		if p.Name == "" || p.Table == "" || p.Command == "" {
			return fmt.Errorf("%w: row policy requires name, table, and command", ErrInvalidState)
		}
	}
	return nil
}

func (s State) Sanitized() State {
	out := s
	out.Principals = append([]Principal(nil), s.Principals...)
	for i := range out.Principals {
		out.Principals[i].AuthRef = ""
	}
	out.Memberships = append([]Membership(nil), s.Memberships...)
	out.Grants = append([]Grant(nil), s.Grants...)
	out.DefaultPrivileges = append([]DefaultPrivilege(nil), s.DefaultPrivileges...)
	out.Policies = append([]RowPolicy(nil), s.Policies...)
	return out
}

func (s State) Digest() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(s.Sanitized())
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

type ChangeKind string

const (
	CreatePrincipal  ChangeKind = "create_principal"
	AlterPrincipal   ChangeKind = "alter_principal"
	DropPrincipal    ChangeKind = "drop_principal"
	GrantAccess      ChangeKind = "grant"
	RevokeAccess     ChangeKind = "revoke"
	MembershipChange ChangeKind = "membership"
	PolicyChange     ChangeKind = "row_policy"
)

type Change struct {
	Kind          ChangeKind  `json:"kind"`
	Principal     *Principal  `json:"principal,omitempty"`
	Before        *Principal  `json:"before,omitempty"`
	Grant         *Grant      `json:"grant,omitempty"`
	Membership    *Membership `json:"membership,omitempty"`
	Policy        *RowPolicy  `json:"policy,omitempty"`
	AffectedPaths []string    `json:"affected_paths,omitempty"`
	Diagnostic    string      `json:"diagnostic,omitempty"`
}

type PlanOptions struct {
	ExecutingPrincipal   string
	EmergencyPrincipals  []string
	AllowExternalChanges bool
}

func Plan(desired, observed State, opts PlanOptions) ([]Change, error) {
	if err := desired.Validate(); err != nil {
		return nil, err
	}
	if err := observed.Validate(); err != nil {
		return nil, err
	}
	if opts.ExecutingPrincipal == "" {
		return nil, fmt.Errorf("%w: executing principal is required", ErrLockout)
	}
	old := map[string]Principal{}
	for _, p := range observed.Principals {
		old[p.Name] = p
	}
	newp := map[string]Principal{}
	for _, p := range desired.Principals {
		newp[p.Name] = p
	}
	var out []Change
	for _, p := range desired.Principals {
		if before, ok := old[p.Name]; !ok {
			out = append(out, Change{Kind: CreatePrincipal, Principal: &p})
		} else if !reflect.DeepEqual(before, p) {
			if before.External && !opts.AllowExternalChanges {
				return nil, fmt.Errorf("%w: %s", ErrExternal, p.Name)
			}
			p.AuthRef = ""
			before.AuthRef = ""
			out = append(out, Change{Kind: AlterPrincipal, Principal: &p, Before: &before})
		}
	}
	for _, p := range observed.Principals {
		if _, ok := newp[p.Name]; !ok {
			if p.External && !opts.AllowExternalChanges {
				continue
			}
			paths := accessPaths(p.Name, observed)
			if p.Name == opts.ExecutingPrincipal || contains(opts.EmergencyPrincipals, p.Name) {
				return nil, fmt.Errorf("%w: cannot drop %s", ErrLockout, p.Name)
			}
			out = append(out, Change{Kind: DropPrincipal, Before: &p, AffectedPaths: paths, Diagnostic: fmt.Sprintf("dropping principal affects %d access paths", len(paths))})
		}
	}
	oldGrants := setGrants(observed.Grants)
	newGrants := setGrants(desired.Grants)
	for key, g := range newGrants {
		if _, ok := oldGrants[key]; !ok {
			gg := g
			out = append(out, Change{Kind: GrantAccess, Grant: &gg})
		}
	}
	for key, g := range oldGrants {
		if _, ok := newGrants[key]; !ok {
			if g.Grantee == opts.ExecutingPrincipal || contains(opts.EmergencyPrincipals, g.Grantee) {
				return nil, fmt.Errorf("%w: revoke %s from %s", ErrLockout, g.Privilege, g.Grantee)
			}
			gg := g
			out = append(out, Change{Kind: RevokeAccess, Grant: &gg, AffectedPaths: accessPaths(g.Grantee, observed)})
		}
	}
	for _, m := range desired.Memberships {
		if !containsMembership(observed.Memberships, m) {
			mm := m
			out = append(out, Change{Kind: MembershipChange, Membership: &mm})
		}
	}
	for _, m := range observed.Memberships {
		if !containsMembership(desired.Memberships, m) {
			if m.Member == opts.ExecutingPrincipal || contains(opts.EmergencyPrincipals, m.Member) {
				return nil, fmt.Errorf("%w: remove membership for %s", ErrLockout, m.Member)
			}
			mm := m
			out = append(out, Change{Kind: MembershipChange, Membership: &mm, AffectedPaths: []string{m.Member + " inherits " + m.Parent}})
		}
	}
	for _, p := range desired.Policies {
		if !containsPolicy(observed.Policies, p) {
			pp := p
			out = append(out, Change{Kind: PolicyChange, Policy: &pp})
		}
	}
	for _, p := range observed.Policies {
		if !containsPolicy(desired.Policies, p) {
			pp := p
			out = append(out, Change{Kind: PolicyChange, Policy: &pp, Diagnostic: "row policy will be removed"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].Kind) < rank(out[j].Kind) })
	return out, nil
}

func rank(k ChangeKind) int {
	switch k {
	case CreatePrincipal:
		return 1
	case AlterPrincipal:
		return 2
	case MembershipChange:
		return 3
	case GrantAccess:
		return 4
	case PolicyChange:
		return 5
	case RevokeAccess:
		return 6
	case DropPrincipal:
		return 7
	}
	return 99
}
func setGrants(in []Grant) map[string]Grant {
	out := map[string]Grant{}
	for _, g := range in {
		out[fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%t", g.Object, g.Grantee, g.Grantor, g.Privilege, g.Grantable)] = g
	}
	return out
}
func containsMembership(in []Membership, x Membership) bool {
	for _, m := range in {
		if m == x {
			return true
		}
	}
	return false
}
func containsPolicy(in []RowPolicy, x RowPolicy) bool {
	for _, p := range in {
		if p.Name == x.Name && p.Table == x.Table && p.Command == x.Command && p.Using == x.Using && p.Check == x.Check && fmt.Sprint(p.Roles) == fmt.Sprint(x.Roles) {
			return true
		}
	}
	return false
}
func contains(in []string, x string) bool {
	for _, v := range in {
		if v == x {
			return true
		}
	}
	return false
}
func accessPaths(name string, s State) []string {
	var out []string
	for _, m := range s.Memberships {
		if m.Member == name {
			out = append(out, m.Member+" inherits "+m.Parent)
		}
	}
	for _, g := range s.Grants {
		if g.Grantee == name {
			out = append(out, g.Privilege+" on "+g.Object)
		}
	}
	sort.Strings(out)
	return out
}

type Rule struct {
	Name        string
	Check       func(State) []Violation
	Remediation string
}
type Violation struct{ Rule, Principal, Object, Message, Remediation string }
type Policy struct {
	Version                 string
	Rules                   []Rule
	ChangeRules             []ChangeRule
	RequireManagedOwnership bool
	ProtectedPrincipals     []string
	Ownership               map[string][]string
}

type ChangeRule struct {
	Name        string
	Check       func([]Change) []Violation
	Remediation string
}

func EvaluatePolicy(s State, p Policy) ([]Violation, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if p.Version == "" {
		p.Version = "security/v1"
	}
	var out []Violation
	for _, r := range p.Rules {
		if r.Check != nil {
			for _, v := range r.Check(s) {
				v.Rule = r.Name
				if v.Remediation == "" {
					v.Remediation = r.Remediation
				}
				out = append(out, v)
			}
		}
	}
	if p.RequireManagedOwnership {
		for _, x := range s.Principals {
			if !x.Managed && !x.External {
				out = append(out, Violation{Rule: "managed-ownership", Principal: x.Name, Message: "principal must be managed or explicitly external", Remediation: "declare ownership or mark external"})
			}
		}
	}
	for actor, objects := range p.Ownership {
		allowed := map[string]bool{}
		for _, object := range objects {
			allowed[object] = true
		}
		for _, g := range s.Grants {
			if g.Grantor == actor && len(allowed) > 0 && !allowed[g.Object] {
				out = append(out, Violation{Rule: "ownership", Principal: actor, Object: g.Object, Message: "actor is outside its ownership boundary", Remediation: "assign the object to the owning team or delegate explicitly"})
			}
		}
	}
	return out, nil
}

// EvaluateChanges applies the same versioned policy to a proposed security
// changeset before it is approved. It is intentionally separate from
// EvaluatePolicy so CI can fail without needing a live database connection.
func EvaluateChanges(changes []Change, p Policy) ([]Violation, error) {
	if p.Version == "" {
		p.Version = "security/v1"
	}
	var out []Violation
	for _, r := range p.ChangeRules {
		if r.Check == nil {
			continue
		}
		for _, v := range r.Check(changes) {
			v.Rule = r.Name
			if v.Remediation == "" {
				v.Remediation = r.Remediation
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// EvaluateDrift uses the observed snapshot path explicitly; keeping this
// entry point named makes drift monitors and CI integrations hard to confuse
// with desired-state evaluation.
func EvaluateDrift(observed State, p Policy) ([]Violation, error) {
	return EvaluatePolicy(observed, p)
}
func NoPublicGrants(s State) []Violation {
	var out []Violation
	for _, g := range s.Grants {
		if strings.EqualFold(g.Grantee, "PUBLIC") {
			out = append(out, Violation{Object: g.Object, Message: "PUBLIC grant is not allowed", Remediation: "grant the minimum privilege to a managed role"})
		}
	}
	return out
}
func RequireRLS(tablePattern string) Rule {
	return Rule{Name: "required-rls", Remediation: "enable tenant RLS and declare a policy", Check: func(s State) []Violation {
		var out []Violation
		for _, g := range s.Grants {
			if strings.Contains(g.Object, tablePattern) {
				found := false
				for _, p := range s.Policies {
					if p.Table == g.Object {
						found = true
					}
				}
				if !found {
					out = append(out, Violation{Principal: g.Grantee, Object: g.Object, Message: "table has access but no row-level policy"})
				}
			}
		}
		return out
	}}
}

type ApplyOptions struct {
	ExecutingPrincipal       string
	RequireFinalVerification bool
}
type Applier interface {
	Apply(context.Context, Change) error
	Inspect(context.Context) (State, error)
}

func Apply(ctx context.Context, a Applier, changes []Change, desired State, opts ApplyOptions) error {
	if a == nil {
		return fmt.Errorf("%w: applier is required", ErrApply)
	}
	if opts.ExecutingPrincipal == "" {
		return fmt.Errorf("%w: executing principal is required", ErrLockout)
	}
	for _, c := range changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.Apply(ctx, c); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrApply, c.Kind, err)
		}
	}
	if opts.RequireFinalVerification {
		got, err := a.Inspect(ctx)
		if err != nil {
			return fmt.Errorf("%w: final inspection: %v", ErrApply, err)
		}
		gd, _ := desired.Digest()
		od, _ := got.Digest()
		if gd != od {
			return fmt.Errorf("%w: final managed state differs (desired %s observed %s)", ErrApply, gd, od)
		}
	}
	return nil
}

// TokenSource supports cloud/IAM or other short-lived database credentials.
type TokenSource interface {
	Token(context.Context) (value string, expiresAt time.Time, err error)
}
type Session struct {
	Ref           secret.Reference
	Source        TokenSource
	Resolver      *secret.Resolver
	RefreshBefore time.Duration
	mu            sync.Mutex
	value         string
	expiresAt     time.Time
}

func (s *Session) Credential(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.value != "" && now.Add(s.RefreshBefore).Before(s.expiresAt) {
		return s.value, nil
	}
	if s.Source != nil {
		v, e, err := s.Source.Token(ctx)
		if err != nil {
			return "", err
		}
		if v == "" || e.IsZero() {
			return "", errors.New("token provider returned empty or non-expiring token")
		}
		s.value = v
		s.expiresAt = e
		return v, nil
	}
	if s.Resolver == nil {
		return "", errors.New("credential resolver is required")
	}
	v, err := s.Resolver.Resolve(ctx, s.Ref)
	if err != nil {
		return "", err
	}
	s.value = v
	s.expiresAt = now.Add(5 * time.Minute)
	return v, nil
}
func (s *Session) Clear() { s.mu.Lock(); s.value = ""; s.expiresAt = time.Time{}; s.mu.Unlock() }
