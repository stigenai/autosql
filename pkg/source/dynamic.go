package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	ErrDynamicPolicy = errors.New("dynamic source policy denied")
	ErrDynamicLimit  = errors.New("dynamic source limit exceeded")
	ErrLockMismatch  = errors.New("dynamic source lock digest mismatch")
	ErrOffline       = errors.New("dynamic source unavailable in offline mode")
)

type DynamicKind string

const (
	KindFile     DynamicKind = "file"
	KindHTTP     DynamicKind = "http"
	KindProgram  DynamicKind = "program"
	KindTemplate DynamicKind = "template"
)

type DynamicSource struct {
	URI         string            `json:"uri"`
	Kind        DynamicKind       `json:"kind"`
	Format      Format            `json:"format"`
	Digest      string            `json:"digest"`
	Command     []string          `json:"command,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Variables   map[string]string `json:"variables,omitempty"`
	MaxBytes    int64             `json:"max_bytes"`
	MaxRows     int               `json:"max_rows,omitempty"`
	Timeout     time.Duration     `json:"timeout"`
	Allowlisted bool              `json:"allowlisted"`
}

type DynamicPolicy struct {
	Offline         bool              `json:"offline"`
	AllowedSchemes  map[string]bool   `json:"allowed_schemes"`
	AllowedPrograms map[string]bool   `json:"allowed_programs"`
	Locks           map[string]string `json:"locks"`
	MaxBytes        int64             `json:"max_bytes"`
	MaxRows         int               `json:"max_rows"`
	Timeout         time.Duration     `json:"timeout"`
}

type Provenance struct {
	URI         string      `json:"uri"`
	Kind        DynamicKind `json:"kind"`
	Digest      string      `json:"digest"`
	RetrievedAt time.Time   `json:"retrieved_at"`
	Locked      bool        `json:"locked"`
}

type Artifact struct {
	Data       []byte     `json:"data"`
	Format     Format     `json:"format"`
	Provenance Provenance `json:"provenance"`
}

type Resolver struct {
	HTTPClient *http.Client
	ReadFile   func(string) ([]byte, error)
	Run        func(context.Context, []string) ([]byte, error)
	Cache      map[string][]byte
	Now        func() time.Time
}

func NewResolver() *Resolver {
	return &Resolver{HTTPClient: &http.Client{}, ReadFile: os.ReadFile, Cache: map[string][]byte{}, Now: time.Now}
}

func (s DynamicSource) Validate(policy DynamicPolicy) error {
	if s.URI == "" || s.Kind == "" || s.Format == "" || s.MaxBytes <= 0 || s.Timeout <= 0 {
		return fmt.Errorf("%w: source URI, kind, format, limits, and timeout are required", ErrDynamicPolicy)
	}
	if !s.Allowlisted {
		return fmt.Errorf("%w: source %s is not allowlisted", ErrDynamicPolicy, s.URI)
	}
	if policy.MaxBytes > 0 && s.MaxBytes > policy.MaxBytes {
		return fmt.Errorf("%w: source max bytes exceeds policy", ErrDynamicLimit)
	}
	if policy.Timeout > 0 && s.Timeout > policy.Timeout {
		return fmt.Errorf("%w: source timeout exceeds policy", ErrDynamicLimit)
	}
	if s.Kind == KindProgram {
		if len(s.Command) == 0 || !policy.AllowedPrograms[s.Command[0]] {
			return fmt.Errorf("%w: program is not allowlisted", ErrDynamicPolicy)
		}
	}
	return nil
}

func (r *Resolver) Resolve(ctx context.Context, s DynamicSource, policy DynamicPolicy) (Artifact, error) {
	if err := s.Validate(policy); err != nil {
		return Artifact{}, err
	}
	if r == nil {
		r = NewResolver()
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if policy.Offline {
		data, ok := r.Cache[s.URI]
		if !ok {
			return Artifact{}, fmt.Errorf("%w: %s", ErrOffline, s.URI)
		}
		return r.finish(s, data, policy, true)
	}
	var data []byte
	var err error
	switch s.Kind {
	case KindFile:
		path := strings.TrimPrefix(s.URI, "file://")
		read := r.ReadFile
		if read == nil {
			read = os.ReadFile
		}
		data, err = read(path)
	case KindHTTP:
		data, err = r.fetch(ctx, s)
	case KindProgram:
		run := r.Run
		if run == nil {
			run = runProgram
		}
		data, err = run(ctx, append([]string(nil), s.Command...))
	case KindTemplate:
		data, err = renderTemplateDir(ctx, strings.TrimPrefix(s.URI, "file://"), s.Variables, s.MaxBytes)
	default:
		return Artifact{}, fmt.Errorf("%w: unsupported dynamic source kind %q", ErrDynamicPolicy, s.Kind)
	}
	if err != nil {
		return Artifact{}, err
	}
	return r.finish(s, data, policy, false)
}

func (r *Resolver) finish(s DynamicSource, data []byte, policy DynamicPolicy, locked bool) (Artifact, error) {
	max := s.MaxBytes
	if policy.MaxBytes > 0 && policy.MaxBytes < max {
		max = policy.MaxBytes
	}
	if int64(len(data)) > max {
		return Artifact{}, fmt.Errorf("%w: %d bytes exceeds %d", ErrDynamicLimit, len(data), max)
	}
	digest := digestBytes(data)
	want := s.Digest
	if want == "" {
		want = policy.Locks[s.URI]
	}
	if want != "" && want != digest {
		return Artifact{}, fmt.Errorf("%w: %s expected %s got %s", ErrLockMismatch, s.URI, want, digest)
	}
	if want != "" {
		locked = true
	}
	return Artifact{Data: append([]byte(nil), data...), Format: s.Format, Provenance: Provenance{URI: s.URI, Kind: s.Kind, Digest: digest, RetrievedAt: r.now(), Locked: locked}}, nil
}

func (r *Resolver) fetch(ctx context.Context, s DynamicSource) ([]byte, error) {
	client := r.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URI, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range s.Headers {
		if strings.EqualFold(k, "authorization") || strings.Contains(strings.ToLower(k), "token") {
			return nil, fmt.Errorf("%w: secret-bearing HTTP header", ErrDynamicPolicy)
		}
		request.Header.Set(k, v)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("dynamic HTTP source returned %s", response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, s.MaxBytes+1))
}

func runProgram(ctx context.Context, command []string) ([]byte, error) {
	if len(command) == 0 {
		return nil, ErrDynamicPolicy
	}
	c := exec.CommandContext(ctx, command[0], command[1:]...)
	return c.Output()
}
func (r *Resolver) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}
func digestBytes(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// DecodeRows converts CSV, JSON, or YAML source output into bounded rows. JSON
// accepts either an array of objects or a single object.
func DecodeRows(data []byte, format Format, maxRows int, maxBytes int64) ([]map[string]any, error) {
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: payload exceeds byte limit", ErrDynamicLimit)
	}
	var rows []map[string]any
	switch format {
	case FormatSQL:
		return nil, fmt.Errorf("%w: SQL is not a row serialization", ErrDynamicPolicy)
	case FormatNative:
		var raw any
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		rows = normalizeRows(raw)
	case Format("yaml"), Format("yml"):
		var raw any
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		normalized := yamlToJSON(raw)
		rows = normalizeRows(normalized)
	case Format("csv"):
		reader := csv.NewReader(bytes.NewReader(data))
		headers, err := reader.Read()
		if err != nil {
			return nil, err
		}
		for i := range headers {
			headers[i] = strings.TrimSpace(headers[i])
		}
		for {
			record, e := reader.Read()
			if errors.Is(e, io.EOF) {
				break
			}
			if e != nil {
				return nil, e
			}
			if len(record) != len(headers) {
				return nil, fmt.Errorf("CSV row has %d fields; expected %d", len(record), len(headers))
			}
			row := map[string]any{}
			for i, h := range headers {
				row[h] = csvValue(record[i])
			}
			rows = append(rows, row)
		}
	default:
		return nil, fmt.Errorf("%w: unsupported row format %q", ErrDynamicPolicy, format)
	}
	if maxRows > 0 && len(rows) > maxRows {
		return nil, fmt.Errorf("%w: %d rows exceeds %d", ErrDynamicLimit, len(rows), maxRows)
	}
	return rows, nil
}

func normalizeRows(raw any) []map[string]any {
	switch x := raw.(type) {
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, v := range x {
			if m, ok := v.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{x}
	default:
		return nil
	}
}
func yamlToJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			out[k] = yamlToJSON(v)
		}
		return out
	case map[any]any:
		out := map[string]any{}
		for k, v := range x {
			out[fmt.Sprint(k)] = yamlToJSON(v)
		}
		return out
	case []any:
		for i := range x {
			x[i] = yamlToJSON(x[i])
		}
		return x
	default:
		return x
	}
}
func csvValue(s string) any {
	if s == "" {
		return nil
	}
	if b, e := strconv.ParseBool(s); e == nil {
		return b
	}
	if i, e := strconv.ParseInt(s, 10, 64); e == nil {
		return i
	}
	if f, e := strconv.ParseFloat(s, 64); e == nil {
		return f
	}
	return s
}

// ComposeJSON overlays JSON objects deterministically. Nested objects merge;
// arrays are replaced by the overlay. This gives environment overlays clear,
// reviewable semantics instead of silently concatenating duplicate resources.
func ComposeJSON(base, overlay []byte) ([]byte, error) {
	var a, b map[string]any
	if err := json.Unmarshal(base, &a); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(overlay, &b); err != nil {
		return nil, err
	}
	mergeJSON(a, b)
	return json.Marshal(a)
}
func mergeJSON(base, overlay map[string]any) {
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := overlay[k]
		if bm, ok := base[k].(map[string]any); ok {
			if om, ok := v.(map[string]any); ok {
				mergeJSON(bm, om)
				continue
			}
		}
		base[k] = v
	}
}

type TemplateInput struct {
	Name string
	Text string
}

func RenderTemplates(inputs []TemplateInput, variables map[string]string, maxBytes int64) ([]byte, error) {
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Name < inputs[j].Name })
	var out bytes.Buffer
	for _, in := range inputs {
		if filepath.IsAbs(in.Name) || strings.Contains(filepath.Clean(in.Name), ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%w: template path %q escapes directory", ErrDynamicPolicy, in.Name)
		}
		t, e := template.New(in.Name).Option("missingkey=error").Parse(in.Text)
		if e != nil {
			return nil, e
		}
		out.WriteString("-- source: " + in.Name + "\n")
		if e = t.Execute(&out, variables); e != nil {
			return nil, e
		}
		out.WriteString("\n")
	}
	if maxBytes > 0 && int64(out.Len()) > maxBytes {
		return nil, ErrDynamicLimit
	}
	return out.Bytes(), nil
}

func renderTemplateDir(ctx context.Context, dir string, variables map[string]string, maxBytes int64) ([]byte, error) {
	var inputs []TemplateInput
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		inputs = append(inputs, TemplateInput{Name: filepath.ToSlash(name), Text: string(data)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return RenderTemplates(inputs, variables, maxBytes)
}
