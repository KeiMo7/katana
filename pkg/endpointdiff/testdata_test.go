package endpointdiff

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTestdata_SampleReport diffs the checked-in two-week sample and asserts
// the rendered report matches the golden file byte-for-byte. This keeps the
// acceptance sample (testdata/README.md) honest: any change to the diffing
// logic that alters output is forced to update the golden file explicitly.
//
// Run with KATANA_REGEN_GOLDEN=1 to regenerate expected.diff.json.
func TestTestdata_SampleReport(t *testing.T) {
	base := filepath.Join("testdata", "week1.jsonl")
	cur := filepath.Join("testdata", "week2.jsonl")
	golden := filepath.Join("testdata", "expected.diff.json")

	manifest := NewManifest(ManifestConfig{MaxDepth: 3, FieldScope: "rdn", Targets: []string{"http://example.local/home"}})
	if err := WriteManifest(ManifestPathFor(base), manifest); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(ManifestPathFor(cur), manifest); err != nil {
		t.Fatal(err)
	}
	// The manifests are written inside the package testdata dir; remove them
	// at the end so only the intended sample artifacts remain checked in.
	defer os.Remove(ManifestPathFor(base))
	defer os.Remove(ManifestPathFor(cur))

	report, err := RunFromFiles(RunConfig{BaselineFile: base, CurrentFile: cur})
	if err != nil {
		t.Fatal(err)
	}
	got, err := MarshalReport(report)
	if err != nil {
		t.Fatal(err)
	}

	if os.Getenv("KATANA_REGEN_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with KATANA_REGEN_GOLDEN=1 to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("sample report differs from golden %s\n--- got\n%s\n--- want\n%s", golden, got, want)
	}
}
