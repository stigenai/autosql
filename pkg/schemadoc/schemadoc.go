// Package schemadoc renders canonical schema graphs into portable review
// artifacts, searchable HTML, and Mermaid ER diagrams.
package schemadoc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"autosql/pkg/schema"
)

var ErrInvalid = errors.New("invalid schema documentation bundle")

type Bundle struct {
	ArtifactDigest string          `json:"artifact_digest"`
	ObservedAt     time.Time       `json:"observed_at"`
	Document       schema.Document `json:"document"`
}

func (b Bundle) Validate() error {
	if b.ArtifactDigest == "" || b.ObservedAt.IsZero() || b.Document.Version == "" {
		return ErrInvalid
	}
	return b.Document.Validate()
}

func (b Bundle) ExportJSON() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(b, "", "  ")
}

func (b Bundle) ERDMermaid() (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}
	var tables []schema.Resource
	for _, r := range b.Document.Graph.Resources {
		if r.Kind == schema.KindTable {
			tables = append(tables, r)
		}
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].ID < tables[j].ID })
	var out strings.Builder
	out.WriteString("erDiagram\n")
	for _, t := range tables {
		fmt.Fprintf(&out, "  %s {\n    string id\n  }\n", mermaidName(t.Name.Name))
	}
	for _, r := range b.Document.Graph.Resources {
		if r.Kind != schema.KindForeignKey || r.Name.Parent == "" {
			continue
		}
		from := resourceName(b.Document, r.Name.Parent)
		to := resourceNameFromSpec(r.Spec)
		if from != "" && to != "" {
			fmt.Fprintf(&out, "  %s }o--|| %s : references\n", mermaidName(from), mermaidName(to))
		}
	}
	return out.String(), nil
}

func resourceName(doc schema.Document, id string) string {
	for _, r := range doc.Graph.Resources {
		if r.ID == id {
			return r.Name.Name
		}
	}
	return ""
}
func resourceNameFromSpec(raw json.RawMessage) string {
	var v struct {
		Target          string `json:"target"`
		Table           string `json:"table"`
		ReferencedTable string `json:"referenced_table"`
	}
	_ = json.Unmarshal(raw, &v)
	if v.Target != "" {
		return v.Target
	}
	if v.ReferencedTable != "" {
		return v.ReferencedTable
	}
	return v.Table
}
func mermaidName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}

var page = template.Must(template.New("schema").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Schema {{.Digest}}</title><style>body{font:14px system-ui;margin:2rem}input{width:24rem;padding:.5rem}li{margin:.25rem 0}.meta{color:#666}</style></head><body><h1>Schema documentation</h1><p class="meta">Artifact <code>{{.Digest}}</code>; observed {{.Observed}}</p><input id="q" placeholder="Search objects"><ul id="objects">{{range .Resources}}<li data-name="{{.Name}}"><code>{{.Kind}}</code> {{.Name}} <small>{{.ID}}</small></li>{{end}}</ul><script>q.oninput=()=>{let x=q.value.toLowerCase();document.querySelectorAll('#objects li').forEach(e=>e.hidden=!e.dataset.name.toLowerCase().includes(x))}</script></body></html>`))

func (b Bundle) HTML() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	type item struct{ ID, Kind, Name string }
	data := struct {
		Digest, Observed string
		Resources        []item
	}{Digest: b.ArtifactDigest, Observed: b.ObservedAt.UTC().Format(time.RFC3339), Resources: []item{}}
	for _, r := range b.Document.Graph.Resources {
		data.Resources = append(data.Resources, item{r.ID, string(r.Kind), r.Name.String()})
	}
	sort.Slice(data.Resources, func(i, j int) bool { return data.Resources[i].Name < data.Resources[j].Name })
	var out bytes.Buffer
	if err := page.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func Compare(previous, current Bundle) (schema.ChangeSet, error) {
	if err := previous.Validate(); err != nil {
		return schema.ChangeSet{}, err
	}
	if err := current.Validate(); err != nil {
		return schema.ChangeSet{}, err
	}
	return schema.Diff(previous.Document, current.Document, schema.DiffOptions{})
}
