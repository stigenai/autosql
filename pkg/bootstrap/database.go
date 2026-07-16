package bootstrap

import (
	"errors"
	"fmt"
	"strings"
)

type DatabaseMode string

const (
	ManagedDatabase  DatabaseMode = "managed"
	ExternalDatabase DatabaseMode = "external"
)

// ServerEndpoint is non-secret connection identity. Runtime credentials and
// complete connection URLs are supplied separately and are never persisted.
type ServerEndpoint struct {
	Host    string `json:"host" yaml:"host"`
	Port    uint16 `json:"port,omitempty" yaml:"port,omitempty"`
	TLSMode string `json:"tls_mode,omitempty" yaml:"tlsMode,omitempty"`
}

// DatabaseTarget declares the cluster-level database boundary separately from
// the schema graph. Encoding and locale fields are immutable after creation;
// changing them requires a new target and a reviewed data migration.
type DatabaseTarget struct {
	Mode                DatabaseMode   `json:"mode" yaml:"mode"`
	Endpoint            ServerEndpoint `json:"endpoint" yaml:"endpoint"`
	MaintenanceDatabase string         `json:"maintenance_database" yaml:"maintenanceDatabase"`
	Name                string         `json:"name" yaml:"name"`
	Owner               string         `json:"owner" yaml:"owner"`
	Encoding            string         `json:"encoding,omitempty" yaml:"encoding,omitempty"`
	LocaleProvider      string         `json:"locale_provider,omitempty" yaml:"localeProvider,omitempty"`
	Collation           string         `json:"collation,omitempty" yaml:"collation,omitempty"`
	CharacterType       string         `json:"character_type,omitempty" yaml:"characterType,omitempty"`
	ICULocale           string         `json:"icu_locale,omitempty" yaml:"icuLocale,omitempty"`
	Template            string         `json:"template,omitempty" yaml:"template,omitempty"`
	Tablespace          string         `json:"tablespace,omitempty" yaml:"tablespace,omitempty"`
	ConnectionLimit     int            `json:"connection_limit" yaml:"connectionLimit"`
	AllowConnections    bool           `json:"allow_connections" yaml:"allowConnections"`
}

func (target DatabaseTarget) Validate() error {
	var problems []error
	if target.Mode != ManagedDatabase && target.Mode != ExternalDatabase {
		problems = append(problems, fmt.Errorf("database mode must be %q or %q", ManagedDatabase, ExternalDatabase))
	}
	for field, value := range map[string]string{"endpoint host": target.Endpoint.Host, "maintenance database": target.MaintenanceDatabase, "database name": target.Name, "owner": target.Owner} {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fmt.Errorf("%s is required", field))
		}
	}
	if strings.ContainsAny(target.Endpoint.Host, "\r\n\x00") {
		problems = append(problems, errors.New("endpoint host contains invalid characters"))
	}
	if target.Endpoint.TLSMode != "" {
		switch target.Endpoint.TLSMode {
		case "disable", "require", "verify-ca", "verify-full":
		default:
			problems = append(problems, errors.New("endpoint TLS mode is unsupported"))
		}
	}
	if target.ConnectionLimit < -1 {
		problems = append(problems, errors.New("connection limit must be -1 or nonnegative"))
	}
	if target.ICULocale != "" && target.LocaleProvider != "icu" {
		problems = append(problems, errors.New("ICU locale requires locale_provider=icu"))
	}
	if target.LocaleProvider != "" && target.LocaleProvider != "libc" && target.LocaleProvider != "icu" && target.LocaleProvider != "builtin" {
		problems = append(problems, errors.New("locale provider must be libc, icu, or builtin"))
	}
	return errors.Join(problems...)
}

// Normalize fills portable PostgreSQL defaults without consulting a server.
func (target DatabaseTarget) Normalize() DatabaseTarget {
	if target.Endpoint.Port == 0 {
		target.Endpoint.Port = 5432
	}
	if target.Endpoint.TLSMode == "" {
		target.Endpoint.TLSMode = "require"
	}
	if target.MaintenanceDatabase == "" {
		target.MaintenanceDatabase = "postgres"
	}
	if target.Encoding == "" {
		target.Encoding = "UTF8"
	}
	if target.Template == "" {
		target.Template = "template0"
	}
	if target.Tablespace == "" {
		target.Tablespace = "pg_default"
	}
	return target
}
