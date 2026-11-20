package endpointdiff

// Manifest is the crawl-scope snapshot written next to a crawl's jsonl
// output. It captures every option that changes which endpoints a crawl can
// observe, so a later comparison can tell "the endpoint disappeared" apart
// from "this run never reached it".
//
// It contains only configuration-derived values and deliberately no
// timestamps or host-specific randomness: two crawls with the same options
// produce byte-identical manifests.
type Manifest struct {
	SchemaVersion int `json:"schema_version"`
	// Targets are the normalized seed URLs (inputs, including stdin).
	Targets []string `json:"targets"`
	// MaxDepth is -d / MaxDepth; 0 means unbounded (crawl-duration mode).
	MaxDepth int `json:"max_depth"`
	// CrawlDuration is the -ct budget in seconds; 0 means depth-bounded only.
	CrawlDurationSeconds int64 `json:"crawl_duration_seconds,omitempty"`
	// Exclude is the -e list (cdn, private-ips, cidr, ip, regex).
	Exclude []string `json:"exclude,omitempty"`
	// Scope / OutOfScope are the -cs / -cos regex lists.
	Scope []string `json:"crawl_scope,omitempty"`
	// OutOfScope is the -cos regex list.
	OutOfScope []string `json:"crawl_out_of_scope,omitempty"`
	// FieldScope is -fs (dn, rdn, fqdn or a custom regex).
	FieldScope string `json:"field_scope"`
	// NoScope is -ns.
	NoScope bool `json:"no_scope,omitempty"`
	// ExtensionsMatch / ExtensionFilter / NoDefaultExtFilter mirror -em/-ef/-ndef.
	ExtensionsMatch    []string `json:"extensions_match,omitempty"`
	ExtensionFilter    []string `json:"extension_filter,omitempty"`
	NoDefaultExtFilter bool     `json:"no_default_ext_filter,omitempty"`
	// IgnoreQueryParams is -iqp.
	IgnoreQueryParams bool `json:"ignore_query_params,omitempty"`
	// MaxDomainPages is -mdp; 0 means unlimited.
	MaxDomainPages int `json:"max_domain_pages,omitempty"`
	// PathClimb is -pc.
	PathClimb bool `json:"path_climb,omitempty"`
}

// Endpoint is one aggregated inventory record: all crawled observations with
// the same identity are merged into it.
type Endpoint struct {
	Identity string
	Method   string
	Class    string
	// URL is the deterministic representative observation: the
	// lexicographically smallest raw URL seen for the identity.
	URL string
	// TemplatedPath is the dynamic-segment-templated path, e.g. /users/{uuid}.
	TemplatedPath string
	// Params is the sorted union of query parameter names.
	Params []string
	// Sources is the sorted union of source page URLs (request.source).
	Sources []string

	// Internal aggregation sets. Unexported so they never serialize; the
	// slices above are materialized deterministically by finalize().
	params  map[string]struct{}
	sources map[string]struct{}
	classes map[string]struct{}
}

// State values for entries only present in the baseline inventory.
const (
	// StateRemoved means the endpoint is genuinely gone: both runs shared
	// the same crawl scope, so the difference cannot be blamed on coverage.
	StateRemoved = "removed"
	// StateNotCovered means the endpoint is absent from the current
	// inventory but the two crawls used different scopes: it may have been
	// removed or simply not reached. It is never reported as a removal.
	StateNotCovered = "not_covered"
)

// Entry is one row in a diff report bucket.
type Entry struct {
	Identity      string   `json:"identity"`
	Method        string   `json:"method"`
	Class         string   `json:"class,omitempty"`
	URL           string   `json:"url"`
	TemplatedPath string   `json:"templated_path"`
	Params        []string `json:"params,omitempty"`
	Sources       []string `json:"sources,omitempty"`
	// State is set on removed-bucket entries: "removed" or "not_covered".
	State string `json:"state,omitempty"`
	// ParamChange is attached when the same identity changed its parameter set.
	ParamChange *ParamChange `json:"param_change,omitempty"`
	// SourceChange is attached when the same identity moved source pages.
	SourceChange *SourceChange `json:"source_change,omitempty"`
}

// ParamChange describes a query parameter set difference on a persisted
// identity. Parameter names only; values never enter the comparison.
type ParamChange struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// SourceChange describes a source-page difference on a persisted identity.
// The endpoint itself is neither added nor removed; its placement on the site
// moved, so it must not be counted as one of each.
type SourceChange struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// RewiredEndpoint is an identity whose route moved: same method, host and
// parameter set, different templated path. Reported instead of an
// add+remove pair.
type RewiredEndpoint struct {
	Identity    string   `json:"identity"`
	Method      string   `json:"method"`
	Class       string   `json:"class,omitempty"`
	OldPath     string   `json:"old_path"`
	NewPath     string   `json:"new_path"`
	Params      []string `json:"params,omitempty"`
	OldSources  []string `json:"old_sources,omitempty"`
	NewSources  []string `json:"new_sources,omitempty"`
	BaselineURL string   `json:"baseline_url"`
	CurrentURL  string   `json:"current_url"`
}

// StructuralChange summarizes a whole-site route reorganization.
type StructuralChange struct {
	// Detected is true when the rewired count meets StructuralChangeThreshold.
	Detected bool `json:"detected"`
	// RewiredCount is the number of endpoint pairs treated as route moves.
	RewiredCount int `json:"rewired_count"`
	// BaselineTotal is the baseline inventory size used for the ratio.
	BaselineTotal int `json:"baseline_total"`
	// Threshold is the ratio (0..1) required to flag Detected.
	Threshold float64           `json:"threshold"`
	Message   string            `json:"message"`
	Rewired   []RewiredEndpoint `json:"rewired"`
}

// ScopeNotice explains that the two crawls used different crawl scopes and
// lists the manifest fields that differ. Baseline-only endpoints are then
// reported as not_covered rather than removed.
type ScopeNotice struct {
	Message         string   `json:"message"`
	DifferingFields []string `json:"differing_fields"`
}

// Summary holds the bucket counts. Invariants:
//
//	baseline_total = removed + not_covered + persisted + rewired
//	current_total  = added   + persisted   + rewired
type Summary struct {
	BaselineTotal int `json:"baseline_total"`
	CurrentTotal  int `json:"current_total"`
	Added         int `json:"added"`
	Removed       int `json:"removed"`
	NotCovered    int `json:"not_covered"`
	Persisted     int `json:"persisted"`
	ParamChanged  int `json:"param_changed"`
	SourceChanged int `json:"source_changed"`
	Rewired       int `json:"rewired"`
}

// Report is the deterministic week-over-week diff artifact. Every slice is
// sorted by identity; no map is serialized, so re-running the comparison on
// the same inputs yields the same bytes.
type Report struct {
	SchemaVersion int `json:"schema_version"`
	// BaselineFile / CurrentFile are the input inventory paths (baseline =
	// older week, current = newer week).
	BaselineFile string `json:"baseline_file"`
	CurrentFile  string `json:"current_file"`
	// ScopeNotice is present when the two crawl manifests differ.
	ScopeNotice *ScopeNotice `json:"scope_notice,omitempty"`
	Summary     Summary      `json:"summary"`
	// StructuralChange is always present (Detected may be false).
	StructuralChange StructuralChange `json:"structural_change"`
	Added            []Entry          `json:"added"`
	Removed          []Entry          `json:"removed"`
	Persisted        []Entry          `json:"persisted"`
}
