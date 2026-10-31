package dbtest

import (
	"net/url"
	"path/filepath"
	"strings"

	"autosql/pkg/secret"
)

// Normalize reference spelling before comparing protected references. Persist
// file references as absolute paths so recovery is independent of working dir.
func (f AdminFactory) normalizeReferences() (AdminFactory, error) {
	base := ""
	if f.Resolver != nil {
		base = f.Resolver.BaseDir
	}
	admin, err := canonicalFactoryReference(f.Definition.Admin, base)
	if err != nil {
		return AdminFactory{}, err
	}
	f.Definition.Admin = admin
	protected := make([]secret.Reference, 0, len(f.ProtectedReferences))
	for _, ref := range f.ProtectedReferences {
		canonical, err := canonicalFactoryReference(ref, base)
		if err != nil {
			return AdminFactory{}, err
		}
		protected = append(protected, canonical)
	}
	f.ProtectedReferences = protected
	return f, nil
}

func canonicalFactoryReference(ref secret.Reference, base string) (secret.Reference, error) {
	if ref.Validate() != nil {
		return "", ErrFactoryConfig
	}
	u, _ := url.Parse(string(ref))
	if u.Scheme == "env" {
		name := u.Host
		if name == "" {
			name = strings.TrimPrefix(u.Path, "/")
		}
		return secret.Reference("env://" + name), nil
	}
	path := u.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", ErrFactoryConfig
	}
	return secret.Reference((&url.URL{Scheme: "file", Path: filepath.Clean(path)}).String()), nil
}
