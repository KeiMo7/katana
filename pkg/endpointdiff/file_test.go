package endpointdiff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func TestManifest_RoundTripAndDeterminism(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.jsonl")
	manifestPath := ManifestPathFor(path)
	if manifestPath != filepath.Join(dir, "run.jsonl.manifest.json") {
		t.Fatalf("manifest path = %q", manifestPath)
	}

	cfg := ManifestConfig{
		Targets:    []string{"https://b.example.com", "https://a.example.com"},
		MaxDepth:   5,
		Exclude:    []string{"cdn", "private-ips"},
		FieldScope: "rdn",
		Scope:      []string{"z.*", "a.*"},
	}
	m1 := NewManifest(cfg)
	cfg.Targets = []string{"https://a.example.com", "https://b.example.com"} // reordered
	m2 := NewManifest(cfg)

	if err := WriteManifest(manifestPath, m1); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := ReadManifest(manifestPath)
	if err != nil || !ok {
		t.Fatalf("ReadManifest: ok=%v err=%v", ok, err)
	}
	if loaded.MaxDepth != 5 || len(loaded.Targets) != 2 || loaded.Targets[0] != "https://a.example.com" {
		t.Fatalf("manifest content mismatch: %+v", loaded)
	}

	// Equivalent configuration in different flag order must serialize equal.
	d1 := mustMarshal(t, m1)
	d2 := mustMarshal(t, m2)
	if string(d1) != string(d2) {
		t.Fatalf("manifests differ despite equivalent configuration:\n%s\n%s", d1, d2)
	}
}

func TestReadManifest_MissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.manifest.json")
	_, ok, err := ReadManifest(missing)
	if err != nil || ok {
		t.Fatalf("missing manifest: ok=%v err=%v", ok, err)
	}
	corrupt := filepath.Join(dir, "bad.manifest.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadManifest(corrupt); err == nil {
		t.Fatal("corrupt manifest must be an error, not a silent miss")
	}
}

func TestWriteReport_AtomicNoTruncatedTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "report.diff.json")
	report := &Report{SchemaVersion: SchemaVersion, Added: []Entry{}, Removed: []Entry{}, Persisted: []Entry{}}

	if err := WriteReport(target, report); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(data)), "}") {
		t.Fatal("report file does not look like complete JSON")
	}
	// No temp files may be left behind in the target directory.
	entries, _ := os.ReadDir(filepath.Dir(target))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".katana-diff-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestWriteAtomic_FailureLeavesTargetUntouched(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "report.json")
	original := []byte(`{"keep":true}` + "\n")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory at the temp-file location forces CreateTemp to fail; the
	// existing target must remain byte-for-byte intact (no half-written file).
	if err := os.MkdirAll(filepath.Join(dir, "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Point the atomic write at a path whose directory is not writable by
	// making the destination directory itself a file.
	fileAsDir := filepath.Join(dir, "actually-a-file")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeAtomic(filepath.Join(fileAsDir, "out.json"), []byte("{}"))
	if err == nil {
		t.Fatal("expected atomic write to fail when the directory is a file")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(original) {
		t.Fatalf("target altered after failed write: %q err=%v", got, err)
	}
}

// End-to-end: two jsonl inventories plus sibling manifests in, deterministic
// report out — exercising the full RunFromFiles path including streaming.
func TestRunFromFiles_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "week1.jsonl")
	curPath := filepath.Join(dir, "week2.jsonl")

	baseLines := strings.Join([]string{
		jsonlLine(t, "GET", "https://api.example.com/v1/users/1", "https://api.example.com/home", nil),
		jsonlLine(t, "GET", "https://api.example.com/v1/gone", "https://api.example.com/home", nil),
	}, "\n") + "\n"
	curLines := strings.Join([]string{
		jsonlLine(t, "GET", "https://api.example.com/v1/users/2", "https://api.example.com/home", nil),
		jsonlLine(t, "GET", "https://api.example.com/v1/fresh", "https://api.example.com/home", nil),
	}, "\n") + "\n"
	if err := os.WriteFile(basePath, []byte(baseLines), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(curPath, []byte(curLines), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := NewManifest(ManifestConfig{MaxDepth: 3, FieldScope: "rdn"})
	if err := WriteManifest(ManifestPathFor(basePath), manifest); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(ManifestPathFor(curPath), manifest); err != nil {
		t.Fatal(err)
	}

	reportPath := filepath.Join(dir, "out.diff.json")
	report1, err := RunFromFiles(RunConfig{BaselineFile: basePath, CurrentFile: curPath, ReportFile: reportPath})
	if err != nil {
		t.Fatal(err)
	}
	// /users/1 and /users/2 share an identity -> persisted, not add/remove.
	if report1.Summary.Persisted != 1 {
		t.Errorf("persisted = %d, want 1 (numeric ids must collapse): %+v", report1.Summary.Persisted, report1.Summary)
	}
	// /gone and /fresh are unrelated paths with no shared transformation, so
	// they remain an ordinary remove+add (not a route rewire).
	if report1.Summary.Added != 1 || report1.Summary.Removed != 1 || report1.Summary.Rewired != 0 {
		t.Errorf("summary = %+v, want 1 added / 1 removed / 0 rewired", report1.Summary)
	}
	if report1.Summary.NotCovered != 0 {
		t.Errorf("not_covered = %d, want 0 with identical manifests", report1.Summary.NotCovered)
	}

	// Re-running must produce byte-identical output at the same path.
	report2, err := RunFromFiles(RunConfig{BaselineFile: basePath, CurrentFile: curPath, ReportFile: reportPath})
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := MarshalReport(report1)
	b2, _ := MarshalReport(report2)
	if string(b1) != string(b2) {
		t.Fatal("repeated diff runs produced different bytes")
	}
	onDisk, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(b2) {
		t.Fatal("report on disk differs from in-memory report")
	}
}

func TestRunFromFiles_MissingBaselineFile(t *testing.T) {
	_, err := RunFromFiles(RunConfig{BaselineFile: "nope.jsonl", CurrentFile: "also-nope.jsonl"})
	if err == nil {
		t.Fatal("expected error for missing baseline file")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := jsonMarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
