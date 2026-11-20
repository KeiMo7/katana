package endpointdiff

import (
	"strings"
	"testing"
)

func manifestsEqual() (*Manifest, *Manifest) {
	m := &Manifest{SchemaVersion: SchemaVersion, MaxDepth: 3, FieldScope: "rdn", Targets: []string{"https://api.example.com"}}
	return m, m
}

func compareOptsEqual() Options {
	a, b := manifestsEqual()
	return Options{BaselineManifest: a, CurrentManifest: b, BaselineFile: "base.jsonl", CurrentFile: "cur.jsonl"}
}

// Scenario 1: pure addition — an endpoint present only in the current run.
func TestCompare_PureAddition(t *testing.T) {
	base := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/users", "https://api.example.com/home", nil),
	)
	cur := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/users", "https://api.example.com/home", nil),
		jsonlLine(t, "POST", "https://api.example.com/v1/orders", "https://api.example.com/home", nil),
	)
	report, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Added != 1 || report.Summary.Removed != 0 || report.Summary.Persisted != 1 {
		t.Fatalf("summary = %+v, want 1 added / 0 removed / 1 persisted", report.Summary)
	}
	if len(report.Added) != 1 || !strings.HasSuffix(report.Added[0].URL, "/v1/orders") {
		t.Fatalf("added bucket = %+v", report.Added)
	}
	if report.ScopeNotice != nil {
		t.Errorf("no scope notice expected when manifests match, got %+v", report.ScopeNotice)
	}
}

// Scenario 2: pure removal — an endpoint gone while crawl scope is identical.
func TestCompare_PureRemoval(t *testing.T) {
	base := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/legacy", "https://api.example.com/home", nil),
		jsonlLine(t, "GET", "https://api.example.com/v1/users", "https://api.example.com/home", nil),
	)
	cur := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/users", "https://api.example.com/home", nil),
	)
	report, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Removed != 1 || report.Summary.NotCovered != 0 {
		t.Fatalf("summary = %+v, want 1 removed / 0 not covered", report.Summary)
	}
	if len(report.Removed) != 1 || report.Removed[0].State != StateRemoved {
		t.Fatalf("removed = %+v, want state %q", report.Removed, StateRemoved)
	}
}

// Acceptance: when crawl scopes differ, baseline-only endpoints are not
// counted as removals — they are not_covered.
func TestCompare_ScopeMismatch_DoesNotCountUncoveredAsRemoved(t *testing.T) {
	base := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/deep/page", "https://api.example.com/home", nil),
	)
	cur := readInventory(t) // empty current inventory
	a, _ := manifestsEqual()
	b := *a
	b.MaxDepth = 1 // current crawl was shallower
	opts := Options{BaselineManifest: a, CurrentManifest: &b, BaselineFile: "base.jsonl", CurrentFile: "cur.jsonl"}

	report, err := Compare(base, cur, opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.ScopeNotice == nil {
		t.Fatal("expected a scope mismatch notice")
	}
	if len(report.ScopeNotice.DifferingFields) != 1 || report.ScopeNotice.DifferingFields[0] != "max_depth" {
		t.Fatalf("differing fields = %v, want [max_depth]", report.ScopeNotice.DifferingFields)
	}
	if report.Summary.Removed != 0 || report.Summary.NotCovered != 1 {
		t.Fatalf("summary = %+v, want 0 removed / 1 not covered", report.Summary)
	}
	if report.Removed[0].State != StateNotCovered {
		t.Errorf("state = %q, want %q", report.Removed[0].State, StateNotCovered)
	}
}

// Scenario 3: parameter structure change on a persisted identity.
func TestCompare_ParameterChange(t *testing.T) {
	base := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/items?page=1", "https://api.example.com/home", []string{"page"}),
	)
	cur := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/items?page=1&limit=20", "https://api.example.com/home", []string{"page", "limit"}),
	)
	report, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Added != 0 || report.Summary.Removed != 0 || report.Summary.Persisted != 1 {
		t.Fatalf("a parameter change must not be an add/remove: summary = %+v", report.Summary)
	}
	if report.Summary.ParamChanged != 1 {
		t.Fatalf("ParamChanged = %d, want 1", report.Summary.ParamChanged)
	}
	pc := report.Persisted[0].ParamChange
	if pc == nil || len(pc.Added) != 1 || pc.Added[0] != "limit" || len(pc.Removed) != 0 {
		t.Fatalf("param change = %+v, want added [limit]", pc)
	}
}

func TestCompare_ParameterRemoval(t *testing.T) {
	base := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/items?a=1&b=2", "https://api.example.com/home", []string{"a", "b"}),
	)
	cur := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/items?a=1", "https://api.example.com/home", []string{"a"}),
	)
	report, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	pc := report.Persisted[0].ParamChange
	if pc == nil || len(pc.Removed) != 1 || pc.Removed[0] != "b" {
		t.Fatalf("param change = %+v, want removed [b]", pc)
	}
}

// Source-page change: same interface mounted on a new page. It must be flagged
// as a source change, not as an add+remove pair.
func TestCompare_SourcePageChange(t *testing.T) {
	base := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/widget", "https://api.example.com/old-page", nil),
	)
	cur := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/widget", "https://api.example.com/new-page", nil),
	)
	report, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Added != 0 || report.Summary.Removed != 0 {
		t.Fatalf("source move must not be add/remove: summary = %+v", report.Summary)
	}
	if report.Summary.SourceChanged != 1 {
		t.Fatalf("SourceChanged = %d, want 1", report.Summary.SourceChanged)
	}
	sc := report.Persisted[0].SourceChange
	if sc == nil || len(sc.Added) != 1 || sc.Added[0] != "https://api.example.com/new-page" ||
		len(sc.Removed) != 1 || sc.Removed[0] != "https://api.example.com/old-page" {
		t.Fatalf("source change = %+v", sc)
	}
}

// Scenario 4: whole-site route reorganization. Many same-shape endpoints move
// to new paths; this must surface as one structural change instead of dozens
// of independent add/remove rows.
func TestCompare_StructuralRouteReorganization(t *testing.T) {
	var baseLines, curLines []string
	// 12 distinct endpoints move from /v1/* to /v2/*, keeping method, host
	// and params — the shape of a whole-site route version migration.
	resourceNames := []string{
		"orders", "users", "invoices", "products", "payments", "refunds",
		"accounts", "sessions", "tokens", "webhooks", "coupons", "subscriptions",
	}
	for _, name := range resourceNames {
		baseLines = append(baseLines, jsonlLine(t, "GET",
			"https://api.example.com/v1/"+name, "https://api.example.com/home", nil))
		curLines = append(curLines, jsonlLine(t, "GET",
			"https://api.example.com/v2/"+name, "https://api.example.com/home", nil))
	}
	base := readInventory(t, baseLines...)
	cur := readInventory(t, curLines...)

	report, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	if !report.StructuralChange.Detected {
		t.Fatalf("expected structural change detection, got %+v", report.StructuralChange)
	}
	if report.StructuralChange.RewiredCount != StructuralChangeMinCount+2 {
		t.Fatalf("rewired = %d, want %d", report.StructuralChange.RewiredCount, StructuralChangeMinCount+2)
	}
	if len(report.Added) != 0 || len(report.Removed) != 0 {
		t.Fatalf("moved routes must not appear as add/remove: added=%d removed=%d", len(report.Added), len(report.Removed))
	}
	first := report.StructuralChange.Rewired[0]
	if first.OldPath != "/v1/accounts" || first.NewPath != "/v2/accounts" {
		t.Errorf("rewire paths = %q -> %q", first.OldPath, first.NewPath)
	}
}

func TestCompare_FewMovesDoNotTriggerStructuralFlag(t *testing.T) {
	base := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/a", "https://api.example.com/home", nil),
		jsonlLine(t, "GET", "https://api.example.com/v1/b", "https://api.example.com/home", nil),
		jsonlLine(t, "GET", "https://api.example.com/v1/c", "https://api.example.com/home", nil),
	)
	cur := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v2/a", "https://api.example.com/home", nil),
		jsonlLine(t, "GET", "https://api.example.com/v1/b", "https://api.example.com/home", nil),
		jsonlLine(t, "GET", "https://api.example.com/v1/c", "https://api.example.com/home", nil),
	)
	report, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	if report.StructuralChange.Detected {
		t.Errorf("a single route move must not flag a structural change: %+v", report.StructuralChange)
	}
	// One isolated path change does not form a transformation batch, so it
	// remains an ordinary add+remove rather than a rewire.
	if report.StructuralChange.RewiredCount != 0 {
		t.Errorf("rewired = %d, want 0 (a lone move has no shared transformation)", report.StructuralChange.RewiredCount)
	}
	if report.Summary.Added != 1 || report.Summary.Removed != 1 {
		t.Errorf("summary = %+v, want the lone move kept as 1 add / 1 remove", report.Summary)
	}
}

// Hard constraint: comparing the same two inventories twice yields byte-
// identical reports.
func TestCompare_DeterministicByteOutput(t *testing.T) {
	base := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/users?a=1", "https://api.example.com/p1", []string{"a"}),
		jsonlLine(t, "POST", "https://api.example.com/v1/orders", "https://api.example.com/p2", nil),
	)
	cur := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/v1/users?a=1&b=2", "https://api.example.com/p3", []string{"a", "b"}),
		jsonlLine(t, "GET", "https://api.example.com/v1/new", "https://api.example.com/p3", nil),
	)
	report1, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	report2, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	b1, err := MarshalReport(report1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := MarshalReport(report2)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatal("two comparisons of the same inputs produced different bytes")
	}
}

// Hard constraint: the three buckets partition the current inventory, and the
// accounting invariants hold: baseline = removed + not_covered + persisted +
// rewired; current = added + persisted + rewired.
func TestCompare_BucketInvariants(t *testing.T) {
	base := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/keep", "https://api.example.com/h", nil),
		jsonlLine(t, "GET", "https://api.example.com/gone", "https://api.example.com/h", nil),
		jsonlLine(t, "GET", "https://api.example.com/v1/move", "https://api.example.com/h", nil),
	)
	cur := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/keep", "https://api.example.com/h", nil),
		jsonlLine(t, "GET", "https://api.example.com/fresh", "https://api.example.com/h", nil),
		jsonlLine(t, "GET", "https://api.example.com/v2/move", "https://api.example.com/h", nil),
	)
	report, err := Compare(base, cur, compareOptsEqual())
	if err != nil {
		t.Fatal(err)
	}
	s := report.Summary
	if s.BaselineTotal != s.Removed+s.NotCovered+s.Persisted+s.Rewired {
		t.Errorf("baseline invariant broken: %+v", s)
	}
	if s.CurrentTotal != s.Added+s.Persisted+s.Rewired {
		t.Errorf("current invariant broken: %+v", s)
	}
	// Three buckets together describe every current endpoint exactly once.
	if s.Added+s.Persisted+s.Rewired != s.CurrentTotal {
		t.Errorf("buckets do not partition the current inventory: %+v", s)
	}
	// The lone /v1/move -> /v2/move change has no transformation batch, so it
	// stays classified as ordinary add+remove (2 added, 2 removed, 1 kept).
	if s.Added != 2 || s.Removed != 2 || s.Persisted != 1 || s.Rewired != 0 {
		t.Errorf("unexpected bucket counts: %+v", s)
	}
}

// Error path: missing baseline manifest is a hard error by default, naming the
// missing artifact rather than silently treating everything as new/removed.
func TestCompare_MissingBaselineManifestFails(t *testing.T) {
	base := readInventory(t, jsonlLine(t, "GET", "https://api.example.com/x", "", nil))
	cur := readInventory(t)
	_, b := manifestsEqual()
	_, err := Compare(base, cur, Options{CurrentManifest: b, BaselineManifestMissing: true})
	if err == nil || !strings.Contains(err.Error(), "manifest is missing") {
		t.Fatalf("expected missing-manifest error, got %v", err)
	}
}

func TestCompare_MissingManifestAllowedBecomesNotice(t *testing.T) {
	base := readInventory(t, jsonlLine(t, "GET", "https://api.example.com/x", "", nil))
	cur := readInventory(t)
	_, b := manifestsEqual()
	report, err := Compare(base, cur, Options{
		CurrentManifest:         b,
		BaselineManifestMissing: true,
		AllowMissingManifest:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.ScopeNotice == nil || !strings.Contains(report.ScopeNotice.Message, "baseline crawl manifest is missing") {
		t.Fatalf("expected scope notice about missing manifest, got %+v", report.ScopeNotice)
	}
	if report.Summary.NotCovered != 1 || report.Summary.Removed != 0 {
		t.Errorf("baseline-only endpoint must be not_covered: %+v", report.Summary)
	}
}
