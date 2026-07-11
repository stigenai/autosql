// Package secret resolves opaque runtime secret references and redacts their values.
package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const maxFileSize = 1 << 20

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Reference identifies a secret without containing its value.
// Supported forms are env://VARIABLE and file:///absolute/or/relative/path.
type Reference string

func Parse(s string) (Reference, error) {
	r := Reference(s)
	if err := r.Validate(); err != nil {
		return "", err
	}
	return r, nil
}

func (r Reference) Validate() error {
	u, err := url.Parse(string(r))
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("invalid secret reference %q", r)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("secret reference must not contain credentials, query, or fragment")
	}
	switch u.Scheme {
	case "env":
		name := envName(u)
		if (u.Host != "" && u.Path != "") || !environmentName.MatchString(name) {
			return fmt.Errorf("invalid environment secret reference %q", r)
		}
	case "file":
		if u.Host != "" && u.Host != "localhost" {
			return fmt.Errorf("file secret reference must not name a remote host")
		}
		if u.Path == "" {
			return fmt.Errorf("file secret reference has no path")
		}
	default:
		return fmt.Errorf("unsupported secret provider %q", u.Scheme)
	}
	return nil
}

func (r Reference) MarshalJSON() ([]byte, error) { return json.Marshal(string(r)) }

type Resolver struct {
	Getenv   func(string) (string, bool)
	ReadFile func(string) ([]byte, error)
	BaseDir  string
	Redactor *Redactor
}

func NewResolver() *Resolver {
	return &Resolver{Getenv: os.LookupEnv, ReadFile: os.ReadFile, Redactor: NewRedactor()}
}

func (r *Resolver) Resolve(ctx context.Context, ref Reference) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := ref.Validate(); err != nil {
		return "", err
	}
	u, _ := url.Parse(string(ref))
	var value string
	switch u.Scheme {
	case "env":
		getenv := r.Getenv
		if getenv == nil {
			getenv = os.LookupEnv
		}
		var ok bool
		value, ok = getenv(envName(u))
		if !ok {
			return "", fmt.Errorf("secret environment variable %s is not set", envName(u))
		}
	case "file":
		path := u.Path
		if !filepath.IsAbs(path) && r.BaseDir != "" {
			path = filepath.Join(r.BaseDir, path)
		}
		read := r.ReadFile
		if read == nil {
			read = os.ReadFile
		}
		b, err := read(path)
		if err != nil {
			return "", fmt.Errorf("read secret file: %w", err)
		}
		if len(b) > maxFileSize {
			return "", fmt.Errorf("secret file exceeds %d bytes", maxFileSize)
		}
		if strings.IndexByte(string(b), 0) >= 0 {
			return "", errors.New("secret file contains NUL byte")
		}
		value = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if value == "" {
		return "", errors.New("resolved secret is empty")
	}
	if r.Redactor != nil {
		r.Redactor.Add(value)
	}
	return value, nil
}

func envName(u *url.URL) string {
	if u.Host != "" {
		return u.Host + strings.TrimPrefix(u.Path, "/")
	}
	return strings.TrimPrefix(u.Path, "/")
}

// Redactor masks known secret values and common encoded representations.
type Redactor struct {
	mu     sync.RWMutex
	values []string
}

func NewRedactor(values ...string) *Redactor {
	r := &Redactor{}
	for _, v := range values {
		r.Add(v)
	}
	return r
}

func (r *Redactor) Add(value string) {
	if value == "" {
		return
	}
	forms := []string{value, url.QueryEscape(value), url.PathEscape(value)}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := make(map[string]bool, len(r.values)+len(forms))
	for _, v := range r.values {
		seen[v] = true
	}
	for _, v := range forms {
		if v != "" && !seen[v] {
			r.values = append(r.values, v)
			seen[v] = true
		}
	}
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

func (r *Redactor) String(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, value := range r.values {
		s = strings.ReplaceAll(s, value, "[REDACTED]")
	}
	return s
}

func (r *Redactor) Writer(w io.Writer) io.Writer { return writer{w: w, r: r} }

type writer struct {
	w io.Writer
	r *Redactor
}

func (w writer) Write(p []byte) (int, error) {
	b := []byte(w.r.String(string(p)))
	_, err := w.w.Write(b)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
