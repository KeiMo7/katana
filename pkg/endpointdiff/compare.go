package endpointdiff

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// StructuralChangeThreshold is the minimum share of baseline endpoints that
// must have moved routes (with at least StructuralChangeMinCount of them)
// before a diff is flagged as a whole-site structural reorganization rather
// than a batch of independent changes.
const (
	StructuralChangeThreshold  = 0.25
	StructuralChangeMinCount   = 10
	manifestFieldMessagePrefix = "crawl scope differs between the two inventories"
)

// Options controls a comparison.
type Options struct {
	// BaselineManifest / CurrentManifest are the crawl-scope snapshots.
	BaselineManifest *Manifest
	CurrentManifest  *Manifest
	// BaselineManifestMissing / CurrentManifestMissing record that the
	// respective inventory had no manifest file. When the baseline one is
	// missing, AllowMissingManifest decides error vs. warning behaviour.
	BaselineManifestMissing bool
	CurrentManifestMissing  bool
	// AllowMissingManifest downgrades a missing baseline manifest from a
	// hard error into a scope warning (baseline-only endpoints then become
	// not_covered, never removed).
	AllowMissingManifest bool
	// BaselineFile / CurrentFile are recorded verbatim in the report.
	BaselineFile string
	CurrentFile  string
}

// Compare computes the deterministic diff between two inventories. It performs
// no IO and starts from in-memory inventories, so the streaming requirement is
// owned by the reader; this function only holds the two aggregated identity
// maps.
func Compare(baseline, current *Inventory, opts Options) (*Report, error) {
	if baseline == nil || current == nil {
		return nil, fmt.Errorf("endpointdiff: both inventories must be non-nil")
	}
	if opts.BaselineManifestMissing && !opts.AllowMissingManifest {
		return nil, fmt.Errorf("endpointdiff: baseline crawl manifest is missing; cannot distinguish removals from uncovered endpoints (re-run with a manifest, or pass -diff-allow-missing-manifest)")
	}

	differing := compareManifests(opts.BaselineManifest, opts.CurrentManifest)
	scopeMismatch := len(differing) > 0 || opts.BaselineManifestMissing || opts.CurrentManifestMissing
	notice := buildScopeNotice(scopeMismatch, differing, opts)

	// Materialize deterministic slices before any comparison reads them.
	baseline.finalizeAll()
	current.finalizeAll()

	bOnly, cOnly, common := partition(baseline, current)
	rewired, bLeft, cLeft := pairRewired(bOnly, cOnly)

	report := &Report{
		SchemaVersion: SchemaVersion,
		BaselineFile:  opts.BaselineFile,
		CurrentFile:   opts.CurrentFile,
		ScopeNotice:   notice,
		Added:         []Entry{},
		Removed:       []Entry{},
		Persisted:     []Entry{},
	}

	// Current-only leftovers are genuine additions.
	for _, ep := range cLeft {
		report.Added = append(report.Added, entryFromEndpoint(ep))
	}

	// Baseline-only leftovers are removals, or not-covered when scopes differ.
	for _, ep := range bLeft {
		entry := entryFromEndpoint(ep)
		if scopeMismatch {
			entry.State = StateNotCovered
		} else {
			entry.State = StateRemoved
		}
		report.Removed = append(report.Removed, entry)
	}

	// Persisted identities: annotate parameter and source-page changes.
	paramChanged, sourceChanged := 0, 0
	for _, id := range common {
		old := baseline.Entries[id]
		new := current.Entries[id]
		entry := entryFromEndpoint(new)
		if pc := diffParams(old, new); pc != nil {
			entry.ParamChange = pc
			paramChanged++
		}
		if sc := diffSources(old, new); sc != nil {
			entry.SourceChange = sc
			sourceChanged++
		}
		report.Persisted = append(report.Persisted, entry)
	}

	detected := len(rewired) >= StructuralChangeMinCount &&
		baselineTotal(baseline) > 0 &&
		float64(len(rewired))/float64(baselineTotal(baseline)) >= StructuralChangeThreshold
	message := ""
	if detected {
		message = fmt.Sprintf("structural change: %d of %d baseline endpoints moved routes (%.0f%%), consistent with a site-wide route reorganization",
			len(rewired), baselineTotal(baseline), 100*float64(len(rewired))/float64(baselineTotal(baseline)))
	}
	report.StructuralChange = StructuralChange{
		Detected:      detected,
		RewiredCount:  len(rewired),
		BaselineTotal: baselineTotal(baseline),
		Threshold:     StructuralChangeThreshold,
		Message:       message,
		Rewired:       rewired,
	}

	report.Summary = Summary{
		BaselineTotal: baselineTotal(baseline),
		CurrentTotal:  len(current.Entries),
		Added:         len(report.Added),
		Removed:       countState(report.Removed, StateRemoved),
		NotCovered:    countState(report.Removed, StateNotCovered),
		Persisted:     len(report.Persisted),
		ParamChanged:  paramChanged,
		SourceChanged: sourceChanged,
		Rewired:       len(rewired),
	}

	sortReport(report)
	return report, nil
}

func baselineTotal(inv *Inventory) int { return len(inv.Entries) }

// partition splits identities into baseline-only, current-only and common,
// each returned as deterministic endpoint slices sorted by identity.
func partition(baseline, current *Inventory) (bOnly, cOnly []*Endpoint, common []string) {
	for id, ep := range baseline.Entries {
		if _, ok := current.Entries[id]; !ok {
			bOnly = append(bOnly, ep)
		} else {
			common = append(common, id)
		}
	}
	for id, ep := range current.Entries {
		if _, ok := baseline.Entries[id]; !ok {
			cOnly = append(cOnly, ep)
		}
	}
	sortEndpoints(bOnly)
	sortEndpoints(cOnly)
	sort.Strings(common)
	return bOnly, cOnly, common
}

// rewireMinBatch is the minimum number of endpoints that must share one
// transformation before their path changes are interpreted as a route move.
// It prevents two unrelated add/remove endpoints from being paired merely
// because they happen to share method, host and parameters.
const rewireMinBatch = 3

// rewireKey groups endpoints eligible to be the same route at a new location:
// same method, same host and same parameter set. The templated path is
// intentionally excluded — a path difference is exactly what a rewire is.
func rewireKey(ep *Endpoint) string {
	return ep.Method + "|" + hostOf(ep.URL) + "|" + strings.Join(ep.Params, ",")
}

// segmentToken maps a dynamic placeholder to a single wildcard so values do
// not leak into the signature.
func segmentToken(seg string) string {
	if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
		return "{*}"
	}
	return seg
}

// pairRewired matches baseline-only and current-only endpoints into route-move
// pairs. A pair counts as a rewire only when at least rewireMinBatch pairs
// share the same rewire key, path shape and segment value mapping (one
// transformation signature); everything else keeps its ordinary add/remove
// classification.
//
// Complexity is O(n log n) with a small depth factor: endpoints are grouped
// linearly by key and shape, pairs are formed by direct map lookups between
// same-shaped paths, and every remaining cost is a sort over at most n items.
// There is no baseline-by-current cross product, so 100k inventories stay
// linear.
func pairRewired(bOnly, cOnly []*Endpoint) (rewired []RewiredEndpoint, bLeft, cLeft []*Endpoint) {
	type pair struct{ old, new *Endpoint }

	bGroups := make(map[string][]*Endpoint)
	cGroups := make(map[string][]*Endpoint)
	for _, ep := range bOnly {
		k := rewireKey(ep)
		bGroups[k] = append(bGroups[k], ep)
	}
	for _, ep := range cOnly {
		k := rewireKey(ep)
		cGroups[k] = append(cGroups[k], ep)
	}

	matchedB := make(map[string]struct{})
	matchedC := make(map[string]struct{})

	groupKeys := make([]string, 0, len(bGroups))
	for k := range bGroups {
		groupKeys = append(groupKeys, k)
	}
	sort.Strings(groupKeys)
	for _, k := range groupKeys {
		bs := bGroups[k]
		cs := cGroups[k]
		if len(bs) == 0 || len(cs) == 0 {
			continue
		}

		// sigPairs groups candidate pairs by their transformation signature.
		sigPairs := make(map[string][]pair)

		// Pair baselines to currents through an inverted segment mapping. A
		// route move keeps the unchanged tail of a path and rewrites one or
		// more leading segments, so current endpoints are indexed by the
		// wildcard-collapsed token vector of their tail. Each baseline then
		// looks up candidates whose tail matches its own: total work is
		// linear in endpoints times path depth, with no cross product.
		type currentEntry struct {
			tokens []string
			ep     *Endpoint
		}
		// tailIndex[length][joined tail tokens from position 1] -> entries.
		tailIndex := make(map[int]map[string][]currentEntry)
		for _, ep := range cs {
			tokens := pathTokens(ep.TemplatedPath)
			if len(tokens) < 2 {
				continue
			}
			byLen := tailIndex[len(tokens)]
			if byLen == nil {
				byLen = make(map[string][]currentEntry)
				tailIndex[len(tokens)] = byLen
			}
			tailKey := joinTokens(tokens[1:])
			byLen[tailKey] = append(byLen[tailKey], currentEntry{tokens: tokens, ep: ep})
		}
		for _, old := range bs {
			oldTokens := pathTokens(old.TemplatedPath)
			byLen := tailIndex[len(oldTokens)]
			if byLen == nil {
				continue
			}
			for _, cand := range byLen[joinTokens(oldTokens[1:])] {
				sig, ok := signatureFromTokens(oldTokens, cand.tokens)
				if !ok {
					continue
				}
				sigPairs[sig] = append(sigPairs[sig], pair{old, cand.ep})
			}
		}

		sigNames := make([]string, 0, len(sigPairs))
		for sig := range sigPairs {
			sigNames = append(sigNames, sig)
		}
		sort.Strings(sigNames)
		for _, sig := range sigNames {
			pairs := sigPairs[sig]
			if len(pairs) < rewireMinBatch {
				continue
			}
			// Deterministic one-to-one matching ordered by baseline then
			// current identity; endpoints consumed by an earlier
			// (lexicographically smaller) signature are skipped.
			sort.Slice(pairs, func(i, j int) bool {
				if pairs[i].old.Identity != pairs[j].old.Identity {
					return pairs[i].old.Identity < pairs[j].old.Identity
				}
				return pairs[i].new.Identity < pairs[j].new.Identity
			})
			usedNew := make(map[string]struct{})
			for _, p := range pairs {
				if _, taken := matchedB[p.old.Identity]; taken {
					continue
				}
				if _, taken := usedNew[p.new.Identity]; taken {
					continue
				}
				if _, taken := matchedC[p.new.Identity]; taken {
					continue
				}
				usedNew[p.new.Identity] = struct{}{}
				rewired = append(rewired, RewiredEndpoint{
					Identity:    p.new.Identity,
					Method:      p.new.Method,
					Class:       p.new.Class,
					OldPath:     p.old.TemplatedPath,
					NewPath:     p.new.TemplatedPath,
					Params:      p.new.Params,
					OldSources:  p.old.Sources,
					NewSources:  p.new.Sources,
					BaselineURL: p.old.URL,
					CurrentURL:  p.new.URL,
				})
				matchedB[p.old.Identity] = struct{}{}
				matchedC[p.new.Identity] = struct{}{}
			}
		}
	}
	for _, ep := range bOnly {
		if _, ok := matchedB[ep.Identity]; !ok {
			bLeft = append(bLeft, ep)
		}
	}
	for _, ep := range cOnly {
		if _, ok := matchedC[ep.Identity]; !ok {
			cLeft = append(cLeft, ep)
		}
	}
	sortEndpoints(bLeft)
	sortEndpoints(cLeft)
	sort.Slice(rewired, func(i, j int) bool { return rewired[i].Identity < rewired[j].Identity })
	return rewired, bLeft, cLeft
}

// signatureFromTokens builds the transformation signature between two equal
// length token vectors: the ordered list of positions whose token changes.
// It returns false when the vectors are identical (no movement happened).
func signatureFromTokens(oldTokens, newTokens []string) (string, bool) {
	if len(oldTokens) != len(newTokens) {
		return "", false
	}
	var changes []string
	changed := false
	for i := range oldTokens {
		if oldTokens[i] != newTokens[i] {
			changed = true
			changes = append(changes, fmt.Sprintf("[%d]%s->%s", i, oldTokens[i], newTokens[i]))
		}
	}
	if !changed {
		return "", false
	}
	return strings.Join(changes, ";"), true
}

func joinTokens(tokens []string) string { return strings.Join(tokens, "\x00") }

// pathTokens splits a templated path into its wildcard-collapsed tokens.
func pathTokens(templatedPath string) []string {
	segs := strings.Split(strings.Trim(templatedPath, "/"), "/")
	for i, seg := range segs {
		segs[i] = segmentToken(seg)
	}
	return segs
}

func diffParams(old, new *Endpoint) *ParamChange {
	added := setDiff(new.Params, old.Params)
	removed := setDiff(old.Params, new.Params)
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	return &ParamChange{Added: added, Removed: removed}
}

func diffSources(old, new *Endpoint) *SourceChange {
	added := setDiff(new.Sources, old.Sources)
	removed := setDiff(old.Sources, new.Sources)
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	return &SourceChange{Added: added, Removed: removed}
}

func entryFromEndpoint(ep *Endpoint) Entry {
	return Entry{
		Identity:      ep.Identity,
		Method:        ep.Method,
		Class:         ep.Class,
		URL:           ep.URL,
		TemplatedPath: ep.TemplatedPath,
		Params:        ep.Params,
		Sources:       ep.Sources,
	}
}

func countState(entries []Entry, state string) int {
	n := 0
	for _, e := range entries {
		if e.State == state {
			n++
		}
	}
	return n
}

func sortEndpoints(eps []*Endpoint) {
	sort.Slice(eps, func(i, j int) bool { return eps[i].Identity < eps[j].Identity })
}

// sortReport fixes the output order of every bucket by identity so the report
// is independent of map iteration and input ordering.
func sortReport(r *Report) {
	sortEntries := func(e []Entry) {
		sort.Slice(e, func(i, j int) bool { return e[i].Identity < e[j].Identity })
	}
	sortEntries(r.Added)
	sortEntries(r.Removed)
	sortEntries(r.Persisted)
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func buildScopeNotice(scopeMismatch bool, differing []string, opts Options) *ScopeNotice {
	if !scopeMismatch {
		return nil
	}
	reasons := make([]string, 0, 2)
	if opts.BaselineManifestMissing {
		reasons = append(reasons, "baseline crawl manifest is missing")
	}
	if opts.CurrentManifestMissing {
		reasons = append(reasons, "current crawl manifest is missing")
	}
	if len(differing) > 0 {
		reasons = append(reasons, fmt.Sprintf("%s: %s", manifestFieldMessagePrefix, strings.Join(differing, ", ")))
	}
	return &ScopeNotice{
		Message:         strings.Join(reasons, "; ") + " — baseline-only endpoints are reported as not_covered, not removed",
		DifferingFields: differing,
	}
}

// compareManifests returns the human-readable labels of manifest fields that
// differ. A nil manifest is treated as an all-zero manifest; the caller
// separately tracks whether a manifest file existed at all.
func compareManifests(a, b *Manifest) []string {
	zero := Manifest{SchemaVersion: SchemaVersion, FieldScope: ""}
	if a == nil {
		a = &zero
	}
	if b == nil {
		b = &zero
	}
	var diffs []string
	check := func(label string, same bool) {
		if !same {
			diffs = append(diffs, label)
		}
	}
	check("max_depth", a.MaxDepth == b.MaxDepth)
	check("crawl_duration", a.CrawlDurationSeconds == b.CrawlDurationSeconds)
	check("exclude", equalStringSlice(a.Exclude, b.Exclude))
	check("crawl_scope", equalStringSlice(a.Scope, b.Scope))
	check("crawl_out_of_scope", equalStringSlice(a.OutOfScope, b.OutOfScope))
	check("field_scope", a.FieldScope == b.FieldScope)
	check("no_scope", a.NoScope == b.NoScope)
	check("extensions_match", equalStringSlice(a.ExtensionsMatch, b.ExtensionsMatch))
	check("extension_filter", equalStringSlice(a.ExtensionFilter, b.ExtensionFilter))
	check("no_default_ext_filter", a.NoDefaultExtFilter == b.NoDefaultExtFilter)
	check("ignore_query_params", a.IgnoreQueryParams == b.IgnoreQueryParams)
	check("max_domain_pages", a.MaxDomainPages == b.MaxDomainPages)
	check("path_climb", a.PathClimb == b.PathClimb)
	check("targets", equalStringSlice(a.Targets, b.Targets))
	return diffs
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca := append([]string(nil), a...)
	cb := append([]string(nil), b...)
	sort.Strings(ca)
	sort.Strings(cb)
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}
