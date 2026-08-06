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
	Promoted     int // matched a resource-site helper call instead of the helper's insides
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

// pendingRef is a func ref pass 1 could not finish: either its origin had no usable
// caller context, or it matched inside a shared schema helper whose args are just the
// helper's parameters (promote names the helper; fallback keeps the inner match in case
// no resource-site call of it resolves).
type pendingRef struct {
	ref                 map[string]any
	origin, field, attr string
	promote             string
	fallback            *callSite
	fallbackSites       int
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
				// a unique match whose args are all bare parameter exprs sits inside a
				// shared schema helper - the literals live at the helper's call sites,
				// so defer and try to promote one level up in pass 2
				if m != nil && m.EnclosingFunc != "" && argsAllExpr(m.Args) {
					pending = append(pending, pendingRef{
						ref: ref, origin: origin, field: fieldForJSON[jsonField], attr: attr,
						promote: m.EnclosingFunc, fallback: m, fallbackSites: sites,
					})
					continue
				}
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

	anchor := majorityFile(votes, 1)

	// pass 2 runs in two rounds: refs that resolve without an anchor (or with the
	// scoped-vote anchor) apply immediately and vote; the failures retry once the vote
	// pool has grown - which is what lets shared-helper promotions disambiguate in
	// resources whose anchor could only come from other pass-2 matches
	runPending := func(list []pendingRef, anchor string, final bool) (requeue []pendingRef) {
		for _, p := range list {
			if p.promote != "" {
				// only a unique or identical-args helper call is trusted with the
				// values; otherwise keep the certain-but-valueless inner match
				if m, sites, _ := e.matchTiers([][]*callSite{e.ix.byCallee[p.promote]}, p.field, p.attr, anchor); m != nil {
					e.apply(p.ref, m, sites, nil)
					e.stats.Promoted++
					if sites == 1 {
						votes[m.File]++
					}
					continue
				}
				if final {
					e.apply(p.ref, p.fallback, p.fallbackSites, nil)
				} else {
					requeue = append(requeue, p)
				}
				continue
			}

			m, sites, cands := e.matchAnchored(p.origin, p.field, p.attr, anchor)
			if m == nil && len(cands) == 0 && !final {
				requeue = append(requeue, p)
				continue
			}
			e.apply(p.ref, m, sites, cands)
			if m != nil && sites == 1 {
				votes[m.File]++
			}
		}
		return requeue
	}

	if requeue := runPending(pending, anchor, false); len(requeue) > 0 {
		retryAnchor := anchor
		if retryAnchor == "" {
			retryAnchor = majorityFile(votes, 2)
		}
		runPending(requeue, retryAnchor, true)
	}

	// the anchor is the file this resource's schema demonstrably lives in - surface it,
	// since wrapper-based providers (azurerm's typed internal/sdk resources, aws's
	// interceptors) report a shared wrapper file for their CRUD refs
	if anchor == "" {
		anchor = majorityFile(votes, 2)
	}
	if anchor != "" {
		rr["detected_file"] = anchor
	}
}

// majorityFile returns the most-voted file, or "" when the winner has fewer than
// minVotes; ties break lexically for determinism.
func majorityFile(votes map[string]int, minVotes int) string {
	file, best := "", 0
	for f, n := range votes {
		if n > best || (n == best && (file == "" || f < file)) {
			file, best = f, n
		}
	}
	if best < minVotes {
		return ""
	}
	return file
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

	// pools, most-specific first: the constructor's own name; the shared-helper name
	// when the origin chain has one (aws's schema.StringLenBetweenSchema.StringLenBetween
	// - the resource-site helper call carries the literals); any call qualified by the
	// origin's package (generic helpers whose callee name the runtime erased)
	pools := [][]*callSite{e.ix.byCallee[callee]}
	if len(elems) >= 2 {
		pools = append(pools, e.ix.byCallee[elems[len(elems)-2]])
	}
	pools = append(pools, e.ix.byQual[pkgBase(pkgPath)])

	return e.matchTiers(pools, wantField, attrName, anchor)
}

// matchTiers narrows each pool through the anchored tiers (resource file, its
// directory, then attr-exact globally) until a pool yields a match or candidates.
func (e *enricher) matchTiers(pools [][]*callSite, wantField, attrName, anchor string) (m *callSite, sites int, cands []*callSite) {
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

	for _, pool := range pools {
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

// argsAllExpr reports whether every arg is an unresolved source expression - the
// signature of a call inside a helper whose real values come from its own callers.
func argsAllExpr(args []any) bool {
	if len(args) == 0 {
		return false
	}
	for _, a := range args {
		m, ok := a.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := m["expr"]; !ok {
			return false
		}
	}
	return true
}

func fieldTieBreak(c []*callSite, wantField string) []*callSite {
	if len(c) > 1 && wantField != "" {
		if sub := filterSites(c, func(cs *callSite) bool {
			return cs.Field == wantField || cs.Field == "return" || cs.Field == "attr_value"
		}); len(sub) > 0 {
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
