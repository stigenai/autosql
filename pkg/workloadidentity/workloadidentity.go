// Package workloadidentity mints short-lived PostgreSQL passwords from cloud
// workload identities. Persisted values describe only the provider, target,
// audience, and subject; token bytes exist only in memory.
package workloadidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type Provider string

const (
	AWSRDS   Provider = "aws_rds_iam"
	GCPCloud Provider = "gcp_cloud_sql_iam"
	AzurePG  Provider = "azure_postgresql_entra"
)

var ErrIdentity = errors.New("workload identity token acquisition failed")

type Binding struct {
	Provider Provider `json:"provider"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	User     string   `json:"user"`
	Database string   `json:"database"`
	TLSMode  string   `json:"tlsMode"`
	Region   string   `json:"region,omitempty"`
	Audience string   `json:"audience"`
	Subject  string   `json:"subject,omitempty"`
}

func (b Binding) Validate() error {
	if b.Provider != AWSRDS && b.Provider != GCPCloud && b.Provider != AzurePG {
		return errors.New("unsupported workload identity provider")
	}
	if strings.TrimSpace(b.Host) == "" || strings.ContainsAny(b.Host, "/@") || net.ParseIP(b.Host) == nil && !strings.Contains(b.Host, ".") {
		return errors.New("database host must be a DNS name or IP address")
	}
	if b.Port < 1 || b.Port > 65535 || strings.TrimSpace(b.User) == "" || strings.ContainsAny(b.User, ":/") || strings.TrimSpace(b.Database) == "" || strings.ContainsAny(b.Database, "/@") {
		return errors.New("database target binding is invalid")
	}
	if b.TLSMode != "require" && b.TLSMode != "verify-ca" && b.TLSMode != "verify-full" {
		return errors.New("workload identity requires TLS")
	}
	if strings.TrimSpace(b.Subject) == "" {
		return errors.New("workload identity subject is required")
	}
	switch b.Provider {
	case AWSRDS:
		if b.Audience != "sts.amazonaws.com" {
			return errors.New("AWS workload identity audience must be sts.amazonaws.com")
		}
	case GCPCloud:
		if !strings.HasPrefix(b.Audience, "//iam.googleapis.com/") {
			return errors.New("GCP workload identity audience is invalid")
		}
	case AzurePG:
		if b.Audience != "api://AzureADTokenExchange" {
			return errors.New("Azure workload identity audience is invalid")
		}
	}
	if b.Provider == AWSRDS && strings.TrimSpace(b.Region) == "" {
		return errors.New("AWS region is required")
	}
	return nil
}

// ConnectionURL returns a transient PostgreSQL URL containing a fresh token.
// Callers must register the returned URL with their redactor and never persist it.
func (s *Source) ConnectionURL(ctx context.Context) (string, time.Time, error) {
	token, expires, err := s.Token(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	u := &url.URL{Scheme: "postgresql", Host: net.JoinHostPort(s.Binding.Host, fmt.Sprint(s.Binding.Port)), Path: "/" + s.Binding.Database, User: url.UserPassword(s.Binding.User, token)}
	q := u.Query()
	q.Set("sslmode", s.Binding.TLSMode)
	u.RawQuery = q.Encode()
	return u.String(), expires, nil
}

func (b Binding) Digest() (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(b)
	h := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

type IssueFunc func(context.Context) (string, time.Time, error)

type Source struct {
	Binding Binding
	issue   IssueFunc
	now     func() time.Time
}

func NewSource(binding Binding, issue IssueFunc) (*Source, error) {
	if err := binding.Validate(); err != nil || issue == nil {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("token issuer is required")
	}
	return &Source{Binding: binding, issue: issue, now: time.Now}, nil
}

func (s *Source) Token(ctx context.Context) (string, time.Time, error) {
	if s == nil || s.issue == nil {
		return "", time.Time{}, ErrIdentity
	}
	value, expires, err := s.issue(ctx)
	if err != nil || value == "" {
		return "", time.Time{}, ErrIdentity
	}
	now := s.now()
	if !expires.After(now.Add(30*time.Second)) || expires.After(now.Add(2*time.Hour)) {
		return "", time.Time{}, ErrIdentity
	}
	return value, expires, nil
}

func NewAWS(ctx context.Context, binding Binding) (*Source, error) {
	if binding.Provider != AWSRDS {
		return nil, errors.New("AWS RDS provider binding required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(binding.Region))
	if err != nil {
		return nil, ErrIdentity
	}
	endpoint := net.JoinHostPort(binding.Host, fmt.Sprint(binding.Port))
	return NewSource(binding, func(ctx context.Context) (string, time.Time, error) {
		token, err := auth.BuildAuthToken(ctx, endpoint, binding.Region, binding.User, cfg.Credentials)
		return token, time.Now().Add(15 * time.Minute), err
	})
}

func NewGCP(ctx context.Context, binding Binding) (*Source, error) {
	if binding.Provider != GCPCloud {
		return nil, errors.New("GCP Cloud SQL provider binding required")
	}
	credentials, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, ErrIdentity
	}
	return newOAuthSource(binding, credentials.TokenSource)
}

func newOAuthSource(binding Binding, source oauth2.TokenSource) (*Source, error) {
	return NewSource(binding, func(context.Context) (string, time.Time, error) {
		token, err := source.Token()
		if err != nil {
			return "", time.Time{}, err
		}
		return token.AccessToken, token.Expiry, nil
	})
}

func NewAzure(binding Binding) (*Source, error) {
	if binding.Provider != AzurePG {
		return nil, errors.New("Azure PostgreSQL provider binding required")
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, ErrIdentity
	}
	return newAzureSource(binding, credential)
}

func newAzureSource(binding Binding, credential azcore.TokenCredential) (*Source, error) {
	return NewSource(binding, func(ctx context.Context) (string, time.Time, error) {
		token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://ossrdbms-aad.database.windows.net/.default"}})
		return token.Token, token.ExpiresOn, err
	})
}
