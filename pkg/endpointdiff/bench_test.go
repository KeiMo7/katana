package endpointdiff

import (
	"fmt"
	"strings"
	"testing"
)

// buildInventory constructs an inventory directly, bypassing JSON encoding,
// so benchmarks measure the diffing logic rather than fixture construction.
// Each resource name is static, so every endpoint keeps a distinct identity.
func buildInventory(b *testing.B, prefix string, n int) *Inventory {
	b.Helper()
	inv := &Inventory{Entries: make(map[string]*Endpoint, n)}
	for i := 0; i < n; i++ {
		raw := fmt.Sprintf("https://api.example.com/v1/%s-res-%d", prefix, i)
		id, err := Identity("GET", raw)
		if err != nil {
			b.Fatal(err)
		}
		inv.Entries[id] = &Endpoint{
			Identity:      id,
			Method:        "GET",
			Class:         "rest",
			URL:           raw,
			TemplatedPath: TemplatePath(raw),
			Params:        nil,
			Sources:       []string{"https://api.example.com/home"},
			params:        map[string]struct{}{},
			sources:       map[string]struct{}{"https://api.example.com/home": {}},
			classes:       map[string]struct{}{"rest": {}},
		}
	}
	return inv
}

// BenchmarkReadInventory_100kSharedIdentity streams a 100,000-line inventory.
// Memory must scale with distinct identities, not file size: every line
// shares one identity here, so allocations stay bounded regardless of count.
func BenchmarkReadInventory_100kSharedIdentity(b *testing.B) {
	line := jsonlLine(b, "GET", "https://api.example.com/v1/items?page=1", "https://api.example.com/home", []string{"page"})
	big := strings.Repeat(line+"\n", 100000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		inv, err := ReadInventory(strings.NewReader(big))
		if err != nil {
			b.Fatal(err)
		}
		if len(inv.Entries) != 1 {
			b.Fatalf("expected 1 identity, got %d", len(inv.Entries))
		}
	}
}

// BenchmarkCompare_100kDistinct measures the full two-inventory comparison at
// the 100k-endpoint scale: half the identities persist, a quarter is removed,
// a quarter is added.
func BenchmarkCompare_100kDistinct(b *testing.B) {
	base := buildInventory(b, "base", 100000)
	// First 50k identities persist; the remaining 50k current entries are new.
	cur := &Inventory{Entries: make(map[string]*Endpoint, 100000)}
	i := 0
	for _, ep := range base.Entries {
		if i < 50000 {
			cur.Entries[ep.Identity] = ep
		}
		i++
	}
	for j := 0; j < 50000; j++ {
		raw := fmt.Sprintf("https://api.example.com/v1/cur-res-%d", j)
		id, _ := Identity("GET", raw)
		cur.Entries[id] = &Endpoint{
			Identity: id, Method: "GET", Class: "rest", URL: raw,
			TemplatedPath: TemplatePath(raw),
			params:        map[string]struct{}{},
			sources:       map[string]struct{}{},
			classes:       map[string]struct{}{"rest": {}},
		}
	}
	a, m := manifestsEqual()
	opts := Options{BaselineManifest: a, CurrentManifest: m}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		report, err := Compare(base, cur, opts)
		if err != nil {
			b.Fatal(err)
		}
		if report.Summary.CurrentTotal != 100000 {
			b.Fatalf("current total = %d", report.Summary.CurrentTotal)
		}
	}
}

// BenchmarkCompare_100kRouteMigration measures the worst case for rewire
// pairing: every baseline endpoint moves to a new path prefix, so all 100k
// pairs participate in one transformation batch.
func BenchmarkCompare_100kRouteMigration(b *testing.B) {
	base := &Inventory{Entries: make(map[string]*Endpoint, 100000)}
	cur := &Inventory{Entries: make(map[string]*Endpoint, 100000)}
	for i := 0; i < 100000; i++ {
		oldRaw := fmt.Sprintf("https://api.example.com/v1/res-%d", i)
		newRaw := fmt.Sprintf("https://api.example.com/v2/res-%d", i)
		oldID, _ := Identity("GET", oldRaw)
		newID, _ := Identity("GET", newRaw)
		base.Entries[oldID] = &Endpoint{
			Identity: oldID, Method: "GET", URL: oldRaw,
			TemplatedPath: TemplatePath(oldRaw),
			params:        map[string]struct{}{}, sources: map[string]struct{}{}, classes: map[string]struct{}{},
		}
		cur.Entries[newID] = &Endpoint{
			Identity: newID, Method: "GET", URL: newRaw,
			TemplatedPath: TemplatePath(newRaw),
			params:        map[string]struct{}{}, sources: map[string]struct{}{}, classes: map[string]struct{}{},
		}
	}
	a, m := manifestsEqual()
	opts := Options{BaselineManifest: a, CurrentManifest: m}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		report, err := Compare(base, cur, opts)
		if err != nil {
			b.Fatal(err)
		}
		if report.StructuralChange.RewiredCount != 100000 {
			b.Fatalf("rewired = %d, want 100000", report.StructuralChange.RewiredCount)
		}
	}
}
