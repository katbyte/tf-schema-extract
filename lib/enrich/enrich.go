// Package enrich implements the --ssa pass: static call-site analysis that recovers the
// literal arguments the src-mode runtime dump cannot see.
//
// The runtime dump identifies each validator/default/diff-suppress func by its closure
// origin (constructor name, usually with the caller inlined into it) but the values
// captured by the closure - the 1 and 100 in validation.IntBetween(1, 100) - only exist
// in source. This pass parses the provider repo (plus any vendored packages that origins
// reference), indexes every constructor call sitting in a schema-relevant position
// (ValidateFunc:, DefaultFunc:, ... or a wrapper's return statement) together with its
// enclosing function, receiver, and nearest attribute key, then matches call sites
// against each func ref's origin and attribute name. Unique matches gain a "call"
// object with the literal args and call file:line; ambiguous ones gain honest
// "call_candidates".
package enrich

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Stats summarises how much the pass managed to recover.
type Stats struct {
	Refs         int // func refs with an origin (closure-built, i.e. potentially has args)
	Matched      int // unique call site found, args attached
	MatchedSame  int // multiple call sites but identical args, args attached
	Ambiguous    int // multiple differing call sites, candidates attached
	Unlocated    int // no call site found (not vendored, or built dynamically)
	FilesScanned int
}

// maxCandidates caps how many differing call sites are attached to an ambiguous ref.
const maxCandidates = 8

// fieldForJSON maps dump func-ref keys to the schema field a matching call site should
// be feeding.
var fieldForJSON = map[string]string{
	"validate_func":      "ValidateFunc",
	"validate_diag_func": "ValidateDiagFunc",
	"default_func":       "DefaultFunc",
	"diff_suppress_func": "DiffSuppressFunc",
	"state_func":         "StateFunc",
	"set_func":           "Set",
}

// Payload enriches a src-mode shim payload in place and returns the re-encoded JSON.
func Payload(payload json.RawMessage, root, modPath string) (json.RawMessage, *Stats, error) {
	ix, err := buildIndex(root, modPath)
	if err != nil {
		return nil, nil, fmt.Errorf("scanning %s: %w", root, err)
	}

	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, nil, fmt.Errorf("decoding src payload: %w", err)
	}

	e := &enricher{ix: ix, stats: &Stats{}}

	if p, ok := doc["provider"].(map[string]any); ok {
		e.schemaMap(p["schema"])
	}
	for _, key := range []string{"resources", "data_sources"} {
		rm, ok := doc[key].(map[string]any)
		if !ok {
			continue
		}
		for _, r := range rm {
			if rr, ok := r.(map[string]any); ok {
				e.schemaMap(rr["schema"])
			}
		}
	}

	e.stats.FilesScanned = ix.filesScanned

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("re-encoding enriched payload: %w", err)
	}

	return out, e.stats, nil
}

type enricher struct {
	ix    *index
	stats *Stats
}

func (e *enricher) schemaMap(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for attrName, av := range m {
		if a, ok := av.(map[string]any); ok {
			e.attribute(a, attrName)
		}
	}
}

func (e *enricher) attribute(a map[string]any, attrName string) {
	for jsonField := range fieldForJSON {
		if ref, ok := a[jsonField].(map[string]any); ok {
			e.ref(ref, jsonField, attrName)
		}
	}

	if el, ok := a["elem"].(map[string]any); ok {
		// Elem: &schema.Schema{...} - a single schema, validators belong to the parent attribute name
		if s, ok := el["schema"].(map[string]any); ok {
			e.attribute(s, attrName)
		}
		if r, ok := el["resource"].(map[string]any); ok {
			e.schemaMap(r["schema"])
		}
	}
}

func (e *enricher) ref(ref map[string]any, jsonField, attrName string) {
	origin, _ := ref["origin"].(string)
	if origin == "" {
		return
	}
	e.stats.Refs++

	match, sites, cands := e.match(origin, fieldForJSON[jsonField], attrName)
	switch {
	case match != nil:
		call := map[string]any{"file": match.File, "line": match.Line, argsKey: match.Args}
		if sites > 1 {
			call["sites"] = sites // identical args found at this many call sites
			e.stats.MatchedSame++
		} else {
			e.stats.Matched++
		}
		ref["call"] = call
	case len(cands) > 0:
		out := make([]any, 0, len(cands))
		for _, c := range cands {
			out = append(out, map[string]any{"file": c.File, "line": c.Line, argsKey: c.Args})
		}
		ref["call_candidates"] = out
		e.stats.Ambiguous++
	default:
		e.stats.Unlocated++
	}
}

// match resolves an origin like
//
//	github.com/../internal/services/containers.schemaNodePoolSysctlConfig.IntBetween
//
// to the call site(s) that could have constructed the closure, narrowing by enclosing
// function (when the caller was inlined into the origin), receiver, attribute name and
// schema field until a unique site remains.
func (e *enricher) match(origin, wantField, attrName string) (m *callSite, sites int, cands []*callSite) {
	pkgPath, elems := parseOrigin(origin)
	if len(elems) == 0 {
		return nil, 0, nil
	}
	callee := elems[len(elems)-1]

	e.ix.ensurePkg(pkgPath)
	c := e.ix.byCallee[callee]

	// caller context in the origin pins the package and enclosing function; a caller-less
	// origin (closure not inlined) falls back to every call of the constructor repo-wide
	if len(elems) >= 2 {
		if sub := filterSites(c, func(cs *callSite) bool {
			return cs.PkgPath == pkgPath && contains(elems[:len(elems)-1], cs.EnclosingFunc)
		}); len(sub) > 0 {
			c = sub
			if len(c) > 1 { // methods sharing a name across types: use the receiver from the origin
				if sub := filterSites(c, func(cs *callSite) bool { return cs.Receiver != "" && contains(elems, cs.Receiver) }); len(sub) > 0 {
					c = sub
				}
			}
		}
	}

	if len(c) > 1 && attrName != "" {
		if sub := filterSites(c, func(cs *callSite) bool { return cs.AttrKey == attrName }); len(sub) > 0 {
			c = sub
		}
	}
	if len(c) > 1 && wantField != "" {
		if sub := filterSites(c, func(cs *callSite) bool { return cs.Field == wantField || cs.Field == "return" }); len(sub) > 0 {
			c = sub
		}
	}

	switch {
	case len(c) == 0:
		return nil, 0, nil
	case len(c) == 1:
		return c[0], 1, nil
	}

	// several sites but identical literal args means the value is still certain
	same := true
	for _, cs := range c[1:] {
		if !reflect.DeepEqual(cs.Args, c[0].Args) {
			same = false
			break
		}
	}
	if same {
		return c[0], len(c), nil
	}

	if len(c) > maxCandidates {
		c = c[:maxCandidates]
	}
	return nil, 0, c
}

func filterSites(in []*callSite, keep func(*callSite) bool) []*callSite {
	var out []*callSite
	for _, cs := range in {
		if keep(cs) {
			out = append(out, cs)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

// parseOrigin splits a fully qualified origin into its package import path and the
// dotted name chain after it, dropping receiver decoration like (*Type).
func parseOrigin(origin string) (pkgPath string, elems []string) {
	lastSlash := strings.LastIndex(origin, "/")
	dot := strings.Index(origin[lastSlash+1:], ".")
	if dot < 0 {
		return origin, nil
	}
	pkgPath = origin[:lastSlash+1+dot]

	for e := range strings.SplitSeq(origin[lastSlash+1+dot+1:], ".") {
		e = strings.Trim(e, "(*)")
		if e != "" {
			elems = append(elems, e)
		}
	}

	return pkgPath, elems
}
