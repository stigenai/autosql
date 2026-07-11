package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

var (
	ErrAmbiguousRename  = errors.New("ambiguous rename hint")
	ErrInvalidSelection = errors.New("invalid schema selection")
)

type RenameHint struct{ From, To string }
type DiffOptions struct {
	Include, Exclude []string
	RenameHints      []RenameHint
}

// SemanticFingerprint returns a stable digest of all document semantics except
// source locations. Unknown extension fields remain part of the digest.
func SemanticFingerprint(doc Document) (string, error) {
	clone, err := cloneDocument(doc)
	if err != nil {
		return "", err
	}
	for i := range clone.Graph.Resources {
		clone.Graph.Resources[i].Source = nil
	}
	clone.Normalize()
	if err := clone.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	return digest("document", raw), nil
}

// ResourceFingerprint returns the same source-independent semantic digest for
// one resource, including unknown fields and dependency metadata.
func ResourceFingerprint(resource Resource) (string, error) {
	clone, err := cloneResource(resource)
	if err != nil {
		return "", err
	}
	clone.Source = nil
	sortDependencies(&clone)
	raw, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	return digest("resource", raw), nil
}

func digest(domain string, raw []byte) string {
	sum := sha256.Sum256(append([]byte("autosql.schema."+domain+"/v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Select applies include patterns with dependency closure, then removes
// excluded objects and every dependent that would otherwise become invalid.
func Select(doc Document, include, exclude []string) (Document, error) {
	for _, pattern := range append(append([]string(nil), include...), exclude...) {
		if _, err := path.Match(pattern, ""); err != nil {
			return Document{}, fmt.Errorf("%w: invalid pattern", ErrInvalidSelection)
		}
	}
	clone, err := cloneDocument(doc)
	if err != nil {
		return Document{}, err
	}
	if err := clone.Validate(); err != nil {
		return Document{}, err
	}
	byID := map[string]Resource{}
	reverse := map[string][]string{}
	for _, r := range clone.Graph.Resources {
		byID[r.ID] = r
		for _, d := range r.Dependencies {
			reverse[d.Target] = append(reverse[d.Target], r.ID)
		}
		if r.Name.Parent != "" {
			reverse[r.Name.Parent] = append(reverse[r.Name.Parent], r.ID)
		}
	}
	keep := map[string]bool{}
	if len(include) == 0 {
		for id := range byID {
			keep[id] = true
		}
	} else {
		for id, r := range byID {
			if matchesAny(r, include) {
				keep[id] = true
			}
		}
	}
	expanded := map[string]bool{}
	var addDeps func(string)
	addDeps = func(id string) {
		r, ok := byID[id]
		if !ok || expanded[id] {
			return
		}
		expanded[id] = true
		keep[id] = true
		if r.Name.Parent != "" {
			addDeps(r.Name.Parent)
		}
		for _, d := range r.Dependencies {
			addDeps(d.Target)
		}
	}
	seeds := make([]string, 0, len(keep))
	for id := range keep {
		seeds = append(seeds, id)
	}
	for _, id := range seeds {
		r := byID[id]
		if r.Name.Parent != "" {
			addDeps(r.Name.Parent)
		}
		for _, d := range r.Dependencies {
			addDeps(d.Target)
		}
	}
	remove := map[string]bool{}
	for id, r := range byID {
		if matchesAny(r, exclude) {
			remove[id] = true
		}
	}
	queue := make([]string, 0, len(remove))
	for id := range remove {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, dependent := range reverse[id] {
			if !remove[dependent] {
				remove[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	var resources []Resource
	for _, r := range clone.Graph.Resources {
		if keep[r.ID] && !remove[r.ID] {
			resources = append(resources, r)
		}
	}
	clone.Graph.Resources = resources
	clone.Normalize()
	if err := clone.Validate(); err != nil {
		return Document{}, fmt.Errorf("%w: %v", ErrInvalidSelection, err)
	}
	return clone, nil
}

func matchesAny(r Resource, patterns []string) bool {
	for _, p := range patterns {
		for _, candidate := range []string{r.ID, r.Name.String(), string(r.Kind) + ":" + r.Name.String()} {
			if ok, _ := path.Match(p, candidate); ok {
				return true
			}
		}
	}
	return false
}

// Diff computes deterministic semantic changes. Inputs must already have been
// normalized by their database driver.
func Diff(current, desired Document, options DiffOptions) (ChangeSet, error) {
	var err error
	current, err = Select(current, options.Include, options.Exclude)
	if err != nil {
		return ChangeSet{}, err
	}
	desired, err = Select(desired, options.Include, options.Exclude)
	if err != nil {
		return ChangeSet{}, err
	}
	cur := resourceMap(current)
	want := resourceMap(desired)
	matchedCur := map[string]bool{}
	matchedWant := map[string]bool{}
	pairs := map[string]string{}
	for id := range cur {
		if _, ok := want[id]; ok {
			pairs[id] = id
			matchedCur[id] = true
			matchedWant[id] = true
		}
	}
	// Explicit hints are the only general rename inference.
	for _, hint := range options.RenameHints {
		from, e := resolveResource(cur, hint.From)
		if e != nil {
			return ChangeSet{}, e
		}
		to, e := resolveResource(want, hint.To)
		if e != nil {
			return ChangeSet{}, e
		}
		if matchedCur[from.ID] || matchedWant[to.ID] || from.Kind != to.Kind {
			return ChangeSet{}, fmt.Errorf("%w: %s -> %s", ErrAmbiguousRename, hint.From, hint.To)
		}
		pairs[from.ID] = to.ID
		matchedCur[from.ID] = true
		matchedWant[to.ID] = true
	}
	// Driver-marked generated names may be ignored only for a unique pair with
	// identical name-independent semantics.
	type bucket struct{ old, new []Resource }
	buckets := map[string]*bucket{}
	for id, r := range cur {
		if !matchedCur[id] && generatedName(r) {
			k := generatedKey(r)
			b := buckets[k]
			if b == nil {
				b = &bucket{}
				buckets[k] = b
			}
			b.old = append(b.old, r)
		}
	}
	for id, r := range want {
		if !matchedWant[id] && generatedName(r) {
			k := generatedKey(r)
			b := buckets[k]
			if b == nil {
				b = &bucket{}
				buckets[k] = b
			}
			b.new = append(b.new, r)
		}
	}
	for _, b := range buckets {
		if len(b.old) == 1 && len(b.new) == 1 {
			a, z := b.old[0], b.new[0]
			equal, _ := generatedEquivalent(a, z)
			if equal {
				matchedCur[a.ID] = true
				matchedWant[z.ID] = true
			}
		}
	}

	var changes []Change
	oldIDs := make([]string, 0, len(pairs))
	for id := range pairs {
		oldIDs = append(oldIDs, id)
	}
	sort.Strings(oldIDs)
	for _, oldID := range oldIDs {
		newID := pairs[oldID]
		before, after := cur[oldID], want[newID]
		if oldID != newID {
			changes = append(changes, newChange(OperationRename, &before, &after))
			continue
		}
		equal, e := resourcesEqual(before, after)
		if e != nil {
			return ChangeSet{}, e
		}
		if !equal {
			changes = append(changes, newChange(OperationAlter, &before, &after))
		}
	}
	for id, r := range want {
		if !matchedWant[id] {
			copy := r
			changes = append(changes, newChange(OperationCreate, nil, &copy))
		}
	}
	for id, r := range cur {
		if !matchedCur[id] {
			copy := r
			changes = append(changes, newChange(OperationDrop, &copy, nil))
		}
	}
	changes = orderChanges(changes, current, desired)
	result := ChangeSet{Version: ChangeVersion, Changes: changes}
	if err := result.Validate(); err != nil {
		return ChangeSet{}, err
	}
	return result, nil
}

func newChange(op Operation, before, after *Resource) Change {
	r := after
	if r == nil {
		r = before
	}
	oldID, newID := "", ""
	if before != nil {
		oldID = before.ID
	}
	if after != nil {
		newID = after.ID
	}
	id := digest("change", []byte(strings.Join([]string{string(op), oldID, newID}, "\x00")))
	return Change{ID: "change:" + strings.TrimPrefix(id, "sha256:")[:24], Operation: op, ResourceID: r.ID, Before: before, After: after}
}
func resourceMap(doc Document) map[string]Resource {
	out := map[string]Resource{}
	for _, r := range doc.Graph.Resources {
		out[r.ID] = r
	}
	return out
}
func resolveResource(resources map[string]Resource, value string) (Resource, error) {
	if r, ok := resources[value]; ok {
		return r, nil
	}
	var found []Resource
	for _, r := range resources {
		if r.Name.String() == value || string(r.Kind)+":"+r.Name.String() == value {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		return Resource{}, fmt.Errorf("%w: %q resolves to %d resources", ErrAmbiguousRename, value, len(found))
	}
	return found[0], nil
}
func generatedName(r Resource) bool  { return r.Annotations["autosql.io/generated-name"] == "true" }
func generatedKey(r Resource) string { return string(r.Kind) + "\x00" + r.Name.Parent }
func generatedEquivalent(a, b Resource) (bool, error) {
	x, e := cloneResource(a)
	if e != nil {
		return false, e
	}
	y, e := cloneResource(b)
	if e != nil {
		return false, e
	}
	x.ID = ""
	y.ID = ""
	x.Name.Name = ""
	y.Name.Name = ""
	x.Source = nil
	y.Source = nil
	delete(x.Annotations, "autosql.io/generated-name")
	delete(y.Annotations, "autosql.io/generated-name")
	sortDependencies(&x)
	sortDependencies(&y)
	xr, _ := json.Marshal(x)
	yr, _ := json.Marshal(y)
	return string(xr) == string(yr), nil
}
func resourcesEqual(a, b Resource) (bool, error) {
	af, e := ResourceFingerprint(a)
	if e != nil {
		return false, e
	}
	bf, e := ResourceFingerprint(b)
	return af == bf, e
}

func orderChanges(changes []Change, current, desired Document) []Change {
	byResource := map[string]string{}
	for _, c := range changes {
		byResource[c.ResourceID] = c.ID
		if c.Before != nil {
			byResource[c.Before.ID] = c.ID
		}
	}
	byChange := map[string]Change{}
	edges := map[string]map[string]bool{}
	indegree := map[string]int{}
	for _, c := range changes {
		byChange[c.ID] = c
		edges[c.ID] = map[string]bool{}
		indegree[c.ID] = 0
	}
	add := func(change, depends string) {
		if change == "" || depends == "" || change == depends || edges[depends][change] {
			return
		}
		edges[depends][change] = true
		indegree[change]++
	}
	for _, c := range changes {
		r := c.After
		if r == nil {
			r = c.Before
		}
		if c.Operation == OperationDrop {
			for _, d := range r.Dependencies {
				add(byResource[d.Target], c.ID)
			}
			if r.Name.Parent != "" {
				add(byResource[r.Name.Parent], c.ID)
			}
		} else {
			for _, d := range r.Dependencies {
				add(c.ID, byResource[d.Target])
			}
			if r.Name.Parent != "" {
				add(c.ID, byResource[r.Name.Parent])
			}
		}
	}
	ready := []string{}
	for id, n := range indegree {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	less := func(a, b string) bool {
		ca, cb := byChange[a], byChange[b]
		ra, rb := changeSortKey(ca), changeSortKey(cb)
		return ra < rb
	}
	sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
	var out []Change
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		c := byChange[id]
		var deps []string
		for parent, nexts := range edges {
			if nexts[id] {
				deps = append(deps, parent)
			}
		}
		sort.Strings(deps)
		c.DependsOn = deps
		out = append(out, c)
		next := make([]string, 0)
		for child := range edges[id] {
			indegree[child]--
			if indegree[child] == 0 {
				next = append(next, child)
			}
		}
		ready = append(ready, next...)
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
	}
	return out
}
func changeSortKey(c Change) string {
	rank := map[Operation]string{OperationDrop: "3", OperationRename: "1", OperationAlter: "1", OperationCreate: "2"}[c.Operation]
	r := c.After
	if r == nil {
		r = c.Before
	}
	return rank + "\x00" + string(r.Kind) + "\x00" + r.Name.String() + "\x00" + c.ID
}

func cloneDocument(doc Document) (Document, error) {
	raw, e := json.Marshal(doc)
	if e != nil {
		return Document{}, e
	}
	var out Document
	e = json.Unmarshal(raw, &out)
	return out, e
}
func cloneResource(resource Resource) (Resource, error) {
	raw, e := json.Marshal(resource)
	if e != nil {
		return Resource{}, e
	}
	var out Resource
	e = json.Unmarshal(raw, &out)
	return out, e
}
func sortDependencies(r *Resource) {
	sort.Slice(r.Dependencies, func(i, j int) bool {
		if r.Dependencies[i].Type == r.Dependencies[j].Type {
			return r.Dependencies[i].Target < r.Dependencies[j].Target
		}
		return r.Dependencies[i].Type < r.Dependencies[j].Type
	})
}
