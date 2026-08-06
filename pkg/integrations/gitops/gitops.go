// Package gitops defines versioned, artifact-bound contracts for CI systems
// and GitOps controllers. Platform adapters render these contracts into their
// native YAML without changing approval or secret-safety semantics.
package gitops

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	Flux        Platform = "flux"
	Crossplane  Platform = "crossplane"
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
	ApprovalDigest string            `json:"approval_digest"`
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
	if c.Platform == "" || c.Version == "" || (c.Mode != Review && c.Mode != Deploy) || c.ArtifactRef == "" || !digest(c.ArtifactDigest) || !digest(c.PolicyDigest) || !digest(c.TargetSnapshot) || c.ApprovalRef == "" || !digest(c.ApprovalDigest) || c.Retry.MaxAttempts <= 0 || c.Retry.Backoff <= 0 {
		return ErrInvalid
	}
	if err := validateOpaqueRef(c.ArtifactRef); err != nil {
		return err
	}
	if err := validateOpaqueRef(c.ApprovalRef); err != nil {
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

func validateOpaqueRef(ref string) error {
	if strings.HasPrefix(ref, "env://") && len(ref) > len("env://") {
		return nil
	}
	if strings.HasPrefix(ref, "file://") && filepath.IsAbs(strings.TrimPrefix(ref, "file://")) {
		return nil
	}
	return ErrCredential
}

// VerifyMaterial resolves only local opaque references and proves their bytes
// match the immutable contract. It returns the artifact path so adapters can
// invoke the real AutoSQL CLI without placing file contents in logs.
func VerifyMaterial(c Contract) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	artifact, err := resolvePath(c.ArtifactRef)
	if err != nil {
		return "", err
	}
	approval, err := resolvePath(c.ApprovalRef)
	if err != nil {
		return "", err
	}
	if err := verifyFileDigest(artifact, c.ArtifactDigest); err != nil {
		return "", err
	}
	if err := verifyFileDigest(approval, c.ApprovalDigest); err != nil {
		return "", err
	}
	return artifact, nil
}

func resolvePath(ref string) (string, error) {
	if strings.HasPrefix(ref, "file://") {
		return strings.TrimPrefix(ref, "file://"), nil
	}
	if strings.HasPrefix(ref, "env://") {
		value := os.Getenv(strings.TrimPrefix(ref, "env://"))
		if value == "" || !filepath.IsAbs(value) {
			return "", ErrCredential
		}
		return value, nil
	}
	return "", ErrCredential
}

func verifyFileDigest(path, expected string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: referenced material unavailable", ErrBinding)
	}
	h := sha256.Sum256(b)
	actual := "sha256:" + hex.EncodeToString(h[:])
	if subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
		return ErrBinding
	}
	return nil
}
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
		return fmt.Sprintf("version: 2.1\njobs:\n  autosql-%s:\n    docker:\n      - image: %s@%s\n    steps:\n      - run: autosql integration run --contract \"$AUTOSQL_CONTRACT\" --contract-digest %q --json\n", c.Mode, c.Image.Name, c.Image.Digest, d), nil
	case Bitbucket:
		return fmt.Sprintf("pipelines:\n  default:\n    - step:\n        name: autosql-%s\n        image: %s@%s\n        oidc: %t\n        script:\n          - autosql integration run --contract \"$AUTOSQL_CONTRACT\" --contract-digest %q --json\n", c.Mode, c.Image.Name, c.Image.Digest, c.OIDC, d), nil
	case AzureDevOps:
		return fmt.Sprintf("steps:\n- script: autosql integration run --contract \"$(AUTOSQL_CONTRACT)\" --contract-digest %q --json\n  displayName: AutoSQL %s\n", d, c.Mode), nil
	case ArgoCD:
		return ArgoApplication(c, "autosql")
	case Flux:
		return fmt.Sprintf("apiVersion: source.toolkit.fluxcd.io/v1\nkind: OCIRepository\nmetadata:\n  name: autosql\nspec:\n  interval: 5m\n  url: oci://%s\n  ref:\n    digest: %s\n---\napiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: autosql\nspec:\n  interval: 5m\n  prune: false\n  sourceRef:\n    kind: OCIRepository\n    name: autosql\n", c.Image.Name, c.Image.Digest), nil
	case Crossplane:
		return fmt.Sprintf("apiVersion: autosql.io/v1alpha1\nkind: AutoSQLSchema\nmetadata:\n  name: autosql\nspec:\n  artifact:\n    ref: %s\n    digest: %s\n  policyDigest: %s\n  targetSnapshot: %s\n  approvalRef: %s\n", c.ArtifactRef, c.ArtifactDigest, c.PolicyDigest, c.TargetSnapshot, c.ApprovalRef), nil
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
