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
	"regexp"
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
const maxCandidates = 4

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
		e.resource(p)
	}
	for _, key := range []string{"resources", "data_sources"} {
		rm, ok := doc[key].(map[string]any)
		if !ok {
			continue
		}
		for _, r := range rm {
			if rr, ok := r.(map[string]any); ok {
				e.resource(rr)
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

// pendingRef is a func ref whose origin had no usable caller context in pass 1.
type pendingRef struct {
	ref                 map[string]any
	origin, field, attr string
}

// resource enriches one resource (or data source, or the provider block) in two passes.
// Pass 1 resolves every ref whose origin carries caller context (constructor inlined
// into a provider function); the files those land in vote for the resource's anchor
// file. Pass 2 resolves the rest (un-inlined constructors, generic validators like
// aws's enum.Validate whose type params the runtime erases) against call sites in the
// anchor file, then its directory. CRUD refs can't provide the anchor - providers like
// aws wrap every resource's CRUD in one shared interceptor.
func (e *enricher) resource(rr map[string]any) {
	var pending []pendingRef
	votes := map[string]int{}

	var collectAttr func(a map[string]any, attr string)
	collectAttr = func(a map[string]any, attr string) {
		for jsonField := range fieldForJSON {
			ref, ok := a[jsonField].(map[string]any)
			if !ok {
				continue
			}
			origin, _ := ref["origin"].(string)
			if origin == "" {
				continue
			}
			e.stats.Refs++

			if m, sites, cands, ok := e.matchScoped(origin, fieldForJSON[jsonField], attr); ok {
				e.apply(ref, m, sites, cands)
				if m != nil && sites == 1 {
					votes[m.File]++
				}
			} else {
				pending = append(pending, pendingRef{ref: ref, origin: origin, field: fieldForJSON[jsonField], attr: attr})
			}
		}

		if el, ok := a["elem"].(map[string]any); ok {
			// Elem: &schema.Schema{...} - a single schema, validators belong to the parent attribute
			if s, ok := el["schema"].(map[string]any); ok {
				collectAttr(s, attr)
			}
			if r, ok := el["resource"].(map[string]any); ok {
				if sm, ok := r["schema"].(map[string]any); ok {
					for an, av := range sm {
						if aa, ok := av.(map[string]any); ok {
							collectAttr(aa, an)
						}
					}
				}
			}
		}
	}

	if sm, ok := rr["schema"].(map[string]any); ok {
		for an, av := range sm {
			if aa, ok := av.(map[string]any); ok {
				collectAttr(aa, an)
			}
		}
	}

	anchor, best := "", 0
	for f, n := range votes {
		if n > best || (n == best && f < anchor) {
			anchor, best = f, n
		}
	}

	for _, p := range pending {
		m, sites, cands := e.matchAnchored(p.origin, p.field, p.attr, anchor)
		e.apply(p.ref, m, sites, cands)
	}
}

// apply attaches the match result to the ref and updates stats.
func (e *enricher) apply(ref map[string]any, match *callSite, sites int, cands []*callSite) {
	switch {
	case match != nil:
		call := map[string]any{"file": match.File, "line": match.Line, argsKey: match.Args}
		if len(match.TypeArgs) > 0 {
			call["type_args"] = match.TypeArgs
		}
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
			cand := map[string]any{"file": c.File, "line": c.Line, argsKey: c.Args}
			if len(c.TypeArgs) > 0 {
				cand["type_args"] = c.TypeArgs
			}
			out = append(out, cand)
		}
		ref["call_candidates"] = out
		e.stats.Ambiguous++
	default:
		e.stats.Unlocated++
	}
}

// matchScoped resolves an origin like
//
//	github.com/../internal/services/containers.schemaNodePoolSysctlConfig.IntBetween
//
// whose caller was inlined into the closure name: the origin pins the package and
// enclosing function, then the match narrows by receiver, attribute name and schema
// field. ok is false when the origin has no caller context (or it matched nothing) and
// the anchored pass should handle the ref instead.
func (e *enricher) matchScoped(origin, wantField, attrName string) (m *callSite, sites int, cands []*callSite, ok bool) {
	pkgPath, elems := parseOrigin(origin)
	if len(elems) < 2 {
		return nil, 0, nil, false
	}
	callee := elems[len(elems)-1]

	e.ix.ensurePkg(pkgPath)

	c := filterSites(e.ix.byCallee[callee], func(cs *callSite) bool {
		return cs.PkgPath == pkgPath && contains(elems[:len(elems)-1], cs.EnclosingFunc)
	})
	if len(c) == 0 {
		return nil, 0, nil, false
	}
	if len(c) > 1 { // methods sharing a name across types: use the receiver from the origin
		if sub := filterSites(c, func(cs *callSite) bool { return cs.Receiver != "" && contains(elems, cs.Receiver) }); len(sub) > 0 {
			c = sub
		}
	}

	// attribute-name and schema-field tie-breakers
	if len(c) > 1 && attrName != "" {
		if sub := filterSites(c, func(cs *callSite) bool { return cs.AttrKey == attrName }); len(sub) > 0 {
			c = sub
		}
	}
	c = fieldTieBreak(c, wantField)

	m, sites, cands = decide(c)
	return m, sites, cands, true
}

// matchAnchored resolves refs without caller context - un-inlined constructors and
// generic helpers whose type params the runtime erases to [...] (aws's enum.Validate).
// Candidates come from the callee name, or failing that from any call whose package
// qualifier matches the origin's package (how enum.Validate[awstypes.X]() is found from
// an enum.validateSlow[...] origin), restricted to the resource's anchor file and then
// its directory. Matching repo-wide without an anchor invites confident wrong answers,
// so no anchor means unlocated.
func (e *enricher) matchAnchored(origin, wantField, attrName, anchor string) (m *callSite, sites int, cands []*callSite) {
	pkgPath, elems := parseOrigin(origin)
	if len(elems) == 0 {
		return nil, 0, nil
	}
	callee := elems[len(elems)-1]

	// anchored tiers: the resource's own file, then its directory; a final global tier
	// requires an exact attribute-key match (a distinctive attribute name pins the call
	// even when the resource produced no anchor at all)
	var tiers []func(*callSite) bool
	if anchor != "" {
		dir := anchor[:strings.LastIndex(anchor, "/")+1]
		tiers = append(tiers,
			func(cs *callSite) bool { return cs.File == anchor },
			func(cs *callSite) bool { return strings.HasPrefix(cs.File, dir) },
		)
	}
	globalTier := len(tiers)
	if attrName != "" {
		tiers = append(tiers, func(cs *callSite) bool { return cs.AttrKey == attrName })
	}

	for _, pool := range [][]*callSite{e.ix.byCallee[callee], e.ix.byQual[pkgBase(pkgPath)]} {
		for i, keep := range tiers {
			c := filterSites(pool, keep)

			// the attribute name is a hard filter in anchored tiers (unknown keys pass,
			// exact keys win): without caller context a same-file call for a different
			// attribute is noise. The global tier is already attr-exact.
			if i < globalTier && attrName != "" {
				c = filterSites(c, func(cs *callSite) bool { return cs.AttrKey == attrName || cs.AttrKey == "" })
				if sub := filterSites(c, func(cs *callSite) bool { return cs.AttrKey == attrName }); len(sub) > 0 {
					c = sub
				}
			}
			c = fieldTieBreak(c, wantField)

			if m, sites, cands = decide(c); m != nil || len(cands) > 0 {
				return m, sites, cands
			}
		}
	}

	return nil, 0, nil
}

func fieldTieBreak(c []*callSite, wantField string) []*callSite {
	if len(c) > 1 && wantField != "" {
		if sub := filterSites(c, func(cs *callSite) bool { return cs.Field == wantField || cs.Field == "return" }); len(sub) > 0 {
			c = sub
		}
	}
	return c
}

// decide turns surviving candidates into a unique match, an identical-args match, or an
// honest candidate list.
func decide(c []*callSite) (m *callSite, sites int, cands []*callSite) {
	switch {
	case len(c) == 0:
		return nil, 0, nil
	case len(c) == 1:
		return c[0], 1, nil
	}

	same := true
	for _, cs := range c[1:] {
		if !reflect.DeepEqual(cs.Args, c[0].Args) || !reflect.DeepEqual(cs.TypeArgs, c[0].TypeArgs) {
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

func pkgBase(pkgPath string) string {
	if i := strings.LastIndex(pkgPath, "/"); i >= 0 {
		return pkgPath[i+1:]
	}
	return pkgPath
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

// genericShapeRE matches the [...] (or [go.shape...]) segments the runtime uses for
// generic instantiations in func names - the concrete type params are erased.
var genericShapeRE = regexp.MustCompile(`\[[^\[\]]*\]`)

// parseOrigin splits a fully qualified origin into its package import path and the
// dotted name chain after it, dropping receiver decoration like (*Type) and erased
// generic type params like validateSlow[...].
func parseOrigin(origin string) (pkgPath string, elems []string) {
	origin = genericShapeRE.ReplaceAllString(origin, "")
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
