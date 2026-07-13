// Package gitops defines versioned, artifact-bound contracts for CI systems
// and GitOps controllers. Platform adapters render these contracts into their
// native YAML without changing approval or secret-safety semantics.
package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosql/pkg/integration"
)

var (
	ErrInvalid    = errors.New("invalid CI/GitOps integration contract")
	ErrBinding    = errors.New("artifact, policy, target, and approval binding mismatch")
	ErrCredential = errors.New("resolved or long-lived credential is not allowed")
)

type Platform string

const (
	CircleCI    Platform = "circleci"
	Bitbucket   Platform = "bitbucket"
	AzureDevOps Platform = "azure-devops"
	ArgoCD      Platform = "argocd"
	GitHub      Platform = "github"
	GitLab      Platform = "gitlab"
)

type Mode string

const (
	Review Mode = "review"
	Deploy Mode = "deploy"
)

type Contract struct {
	Platform       Platform          `json:"platform"`
	Version        string            `json:"version"`
	Mode           Mode              `json:"mode"`
	ArtifactRef    string            `json:"artifact_ref"`
	ArtifactDigest string            `json:"artifact_digest"`
	PolicyDigest   string            `json:"policy_digest"`
	TargetSnapshot string            `json:"target_snapshot"`
	ApprovalRef    string            `json:"approval_ref"`
	OIDC           bool              `json:"oidc"`
	Image          integration.Image `json:"image"`
	Retry          RetryPolicy       `json:"retry"`
}
type RetryPolicy struct {
	MaxAttempts int           `json:"max_attempts"`
	Backoff     time.Duration `json:"backoff"`
	Retryable   []string      `json:"retryable"`
}

func (c Contract) Validate() error {
	if c.Platform == "" || c.Version == "" || (c.Mode != Review && c.Mode != Deploy) || c.ArtifactRef == "" || !digest(c.ArtifactDigest) || !digest(c.PolicyDigest) || !digest(c.TargetSnapshot) || c.ApprovalRef == "" || c.Retry.MaxAttempts <= 0 || c.Retry.Backoff <= 0 {
		return ErrInvalid
	}
	if strings.Contains(c.ArtifactRef, "://") && !strings.HasPrefix(c.ArtifactRef, "env://") && !strings.HasPrefix(c.ArtifactRef, "file://") {
		return ErrCredential
	}
	if c.Mode == Deploy && !c.OIDC {
		return fmt.Errorf("%w: deployment requires OIDC", ErrCredential)
	}
	if err := c.Image.Validate(); err != nil {
		return err
	}
	return nil
}
func digest(s string) bool { return len(s) == 71 && strings.HasPrefix(s, "sha256:") }
func (c Contract) BindingDigest() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

type Status string

const (
	StatusPending  Status = "pending"
	StatusRunning  Status = "running"
	StatusPassed   Status = "passed"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

type Check struct {
	ContractDigest string    `json:"contract_digest"`
	Status         Status    `json:"status"`
	Message        string    `json:"message,omitempty"`
	At             time.Time `json:"at"`
}

func (c Check) Validate(contract Contract) error {
	d, e := contract.BindingDigest()
	if e != nil {
		return e
	}
	if c.ContractDigest != d {
		return ErrBinding
	}
	switch c.Status {
	case StatusPending, StatusRunning, StatusPassed, StatusFailed, StatusCanceled:
		return nil
	}
	return ErrInvalid
}
func Render(c Contract) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	d, _ := c.BindingDigest()
	switch c.Platform {
	case CircleCI:
		return fmt.Sprintf("version: 2.1\njobs:\n  autosql-%s:\n    docker:\n      - image: %s@%s\n    steps:\n      - run: autosql verify --artifact-ref %q --artifact-digest %q --policy-digest %q --target-snapshot %q --approval-ref %q\n", c.Mode, c.Image.Name, c.Image.Digest, c.ArtifactRef, c.ArtifactDigest, c.PolicyDigest, c.TargetSnapshot, c.ApprovalRef), nil
	case Bitbucket:
		return fmt.Sprintf("pipelines:\n  default:\n    - step:\n        name: autosql-%s\n        image: %s@%s\n        oidc: %t\n        script:\n          - autosql verify --contract-digest %q\n", c.Mode, c.Image.Name, c.Image.Digest, c.OIDC, d), nil
	case AzureDevOps:
		return fmt.Sprintf("steps:\n- script: autosql verify --contract-digest %q\n  displayName: AutoSQL %s\n", d, c.Mode), nil
	case ArgoCD:
		return ArgoApplication(c, "autosql")
	case GitHub, GitLab:
		return fmt.Sprintf("# autosql contract %s\n# artifact=%s policy=%s target=%s approval=%s\n", d, c.ArtifactDigest, c.PolicyDigest, c.TargetSnapshot, c.ApprovalRef), nil
	default:
		return "", fmt.Errorf("%w: unsupported platform %s", ErrInvalid, c.Platform)
	}
}
func ArgoApplication(c Contract, name string) (string, error) {
	if c.Platform != ArgoCD {
		return "", ErrInvalid
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf("apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata:\n  name: %s\nspec:\n  syncPolicy:\n    automated: null\n  source:\n    path: .\n  info:\n  - name: autosql-artifact-digest\n    value: %s\n  - name: autosql-policy-digest\n    value: %s\n  - name: autosql-target-snapshot\n    value: %s\n", name, c.ArtifactDigest, c.PolicyDigest, c.TargetSnapshot), nil
}
