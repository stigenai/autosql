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
	children := map[string][]string{}
	for _, r := range clone.Graph.Resources {
		byID[r.ID] = r
		for _, d := range r.Dependencies {
			reverse[d.Target] = append(reverse[d.Target], r.ID)
		}
		if r.Name.Parent != "" {
			reverse[r.Name.Parent] = append(reverse[r.Name.Parent], r.ID)
			children[r.Name.Parent] = append(children[r.Name.Parent], r.ID)
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
	// Selecting a container selects its eligible descendants. Explicit
	// exclusions are applied below and still remove dependent resources.
	var addChildren func(string)
	addChildren = func(id string) {
		for _, child := range children[id] {
			if !keep[child] {
				keep[child] = true
				addChildren(child)
			}
		}
	}
	childSeeds := make([]string, 0, len(keep))
	for id := range keep {
		childSeeds = append(childSeeds, id)
	}
	for _, id := range childSeeds {
		addChildren(id)
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
	// Resolve every hint before validation so child hints may refer to an
	// explicitly renamed parent regardless of hint order.
	type resolvedHint struct {
		hint     RenameHint
		from, to Resource
	}
	resolved := make([]resolvedHint, 0, len(options.RenameHints))
	hintedParents := map[string]string{}
	for _, hint := range options.RenameHints {
		from, e := resolveResource(cur, hint.From)
		if e != nil {
			return ChangeSet{}, e
		}
		to, e := resolveResource(want, hint.To)
		if e != nil {
			return ChangeSet{}, e
		}
		if from.Kind != to.Kind {
			return ChangeSet{}, fmt.Errorf("%w: %s -> %s", ErrAmbiguousRename, hint.From, hint.To)
		}
		if previous, exists := hintedParents[from.ID]; exists && previous != to.ID {
			return ChangeSet{}, fmt.Errorf("%w: conflicting source %s", ErrAmbiguousRename, hint.From)
		}
		hintedParents[from.ID] = to.ID
		resolved = append(resolved, resolvedHint{hint: hint, from: from, to: to})
	}
	seenTargets := map[string]bool{}
	for _, item := range resolved {
		from, to, hint := item.from, item.to, item.hint
		if matchedCur[from.ID] || matchedWant[to.ID] || seenTargets[to.ID] {
			return ChangeSet{}, fmt.Errorf("%w: %s -> %s", ErrAmbiguousRename, hint.From, hint.To)
		}
		parentMapped := from.Name.Parent != "" && hintedParents[from.Name.Parent] == to.Name.Parent
		sameContainer := from.Name.Parent == to.Name.Parent && from.Name.Catalog == to.Name.Catalog && from.Name.Schema == to.Name.Schema
		if !sameContainer && !parentMapped {
			return ChangeSet{}, fmt.Errorf("%w: cross-parent rename %s -> %s", ErrAmbiguousRename, hint.From, hint.To)
		}
		pairs[from.ID] = to.ID
		matchedCur[from.ID] = true
		matchedWant[to.ID] = true
		seenTargets[to.ID] = true
	}
	// A renamed container safely carries same-identity descendants with it.
	// This is repeated for nested resources.
	for changed := true; changed; {
		changed = false
		for oldID, from := range cur {
			if matchedCur[oldID] || from.Name.Parent == "" {
				continue
			}
			newParent, parentRenamed := pairs[from.Name.Parent]
			if !parentRenamed || newParent == from.Name.Parent {
				continue
			}
			var candidates []Resource
			for newID, to := range want {
				if matchedWant[newID] || from.Kind != to.Kind || to.Name.Parent != newParent {
					continue
				}
				if from.Name.Name == to.Name.Name {
					candidates = append(candidates, to)
				}
			}
			if len(candidates) == 1 {
				to := candidates[0]
				pairs[from.ID] = to.ID
				matchedCur[from.ID] = true
				matchedWant[to.ID] = true
				changed = true
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
			intermediate := remapResourceReferences(before, pairs)
			intermediate.ID = after.ID
			intermediate.Name = after.Name
			rename, e := newChange(OperationRename, &before, &intermediate)
			if e != nil {
				return ChangeSet{}, e
			}
			changes = append(changes, rename)
			equal, e := resourcesEqual(intermediate, after)
			if e != nil {
				return ChangeSet{}, e
			}
			if !equal {
				alter, e := newChange(OperationAlter, &intermediate, &after)
				if e != nil {
					return ChangeSet{}, e
				}
				changes = append(changes, alter)
			}
			continue
		}
		equal, e := resourcesEqual(before, after)
		if e != nil {
			return ChangeSet{}, e
		}
		if !equal {
			change, e := newChange(OperationAlter, &before, &after)
			if e != nil {
				return ChangeSet{}, e
			}
			changes = append(changes, change)
		}
	}
	for id, r := range want {
		if !matchedWant[id] {
			copy := r
			change, e := newChange(OperationCreate, nil, &copy)
			if e != nil {
				return ChangeSet{}, e
			}
			changes = append(changes, change)
		}
	}
	for id, r := range cur {
		if !matchedCur[id] {
			copy := r
			change, e := newChange(OperationDrop, &copy, nil)
			if e != nil {
				return ChangeSet{}, e
			}
			changes = append(changes, change)
		}
	}
	changes = orderChanges(changes, current, desired)
	result := ChangeSet{Version: ChangeVersion, Changes: changes}
	if err := result.Validate(); err != nil {
		return ChangeSet{}, err
	}
	return result, nil
}

func remapResourceReferences(resource Resource, pairs map[string]string) Resource {
	if mapped, ok := pairs[resource.Name.Parent]; ok {
		resource.Name.Parent = mapped
	}
	for idx := range resource.Dependencies {
		if mapped, ok := pairs[resource.Dependencies[idx].Target]; ok {
			resource.Dependencies[idx].Target = mapped
		}
	}
	return resource
}

func newChange(op Operation, before, after *Resource) (Change, error) {
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
	beforeFingerprint, afterFingerprint := "", ""
	var err error
	if before != nil {
		beforeFingerprint, err = ResourceFingerprint(*before)
		if err != nil {
			return Change{}, err
		}
	}
	if after != nil {
		afterFingerprint, err = ResourceFingerprint(*after)
		if err != nil {
			return Change{}, err
		}
	}
	id := digest("change", []byte(strings.Join([]string{string(op), oldID, newID, beforeFingerprint, afterFingerprint}, "\x00")))
	return Change{ID: "change:" + strings.TrimPrefix(id, "sha256:")[:24], Operation: op, ResourceID: r.ID, Before: before, After: after}, nil
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
	// A rename followed by an alteration of the renamed object is two explicit
	// operations; execution must never observe the alteration under the old ID.
	renameByAfter := map[string]string{}
	for _, c := range changes {
		if c.Operation == OperationRename && c.After != nil {
			renameByAfter[c.After.ID] = c.ID
		}
	}
	for _, c := range changes {
		if c.Operation == OperationAlter && c.Before != nil {
			add(c.ID, renameByAfter[c.Before.ID])
		}
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
	if e != nil {
		return Document{}, e
	}
	return out, nil
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
