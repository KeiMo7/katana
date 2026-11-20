package endpointdiff

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// manifestSuffix is appended to an inventory path to derive its manifest.
const manifestSuffix = ".manifest.json"

// ManifestPathFor returns the sibling manifest path for an inventory file:
// "run.jsonl" -> "run.jsonl.manifest.json", "run" -> "run.manifest.json".
func ManifestPathFor(inventoryPath string) string {
	return inventoryPath + manifestSuffix
}

// ReadManifest loads a crawl manifest. The boolean is false when no manifest
// exists at path (a readable, parseable manifest returns true). A corrupt
// manifest is an error: silently treating it as absent would hide the
// coverage-vs-removal distinction this file exists to provide.
func ReadManifest(path string) (*Manifest, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, fmt.Errorf("parsing manifest %s: %w", path, err)
	}
	return &m, true, nil
}

// WriteManifest serializes the manifest atomically next to its inventory.
func WriteManifest(path string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

// MarshalReport renders the report as indented JSON. It contains no maps or
// timestamps, so identical reports render to identical bytes.
func MarshalReport(r *Report) ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// WriteReport writes the report atomically: a temp file is fully written,
// fsynced and renamed into place, so a failure mid-comparison or mid-write
// never leaves a truncated report at the target path.
func WriteReport(path string, r *Report) error {
	data, err := MarshalReport(r)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// writeAtomic writes data to path via a temp file in the same directory and
// an atomic rename. On any failure the temp file is removed and path is left
// untouched.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".katana-diff-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("writing temporary file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("syncing temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("renaming temporary file into place: %w", err)
	}
	return nil
}

// ReadInventoryFile opens path and streams it into an Inventory.
func ReadInventoryFile(path string) (*Inventory, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadInventory(f)
}

// ManifestConfig is the flat set of crawl options needed to build a manifest.
// It is defined here (rather than importing pkg/types) to keep endpointdiff
// independent of the crawler's option types; the runner adapts its options
// into it.
type ManifestConfig struct {
	Targets              []string
	MaxDepth             int
	CrawlDurationSeconds int64
	Exclude              []string
	Scope                []string
	OutOfScope           []string
	FieldScope           string
	NoScope              bool
	ExtensionsMatch      []string
	ExtensionFilter      []string
	NoDefaultExtFilter   bool
	IgnoreQueryParams    bool
	MaxDomainPages       int
	PathClimb            bool
}

// NewManifest builds a deterministic manifest from crawl configuration. All
// slices are copied and sorted, so two runs with equivalent configuration
// produce identical bytes regardless of flag/input order.
func NewManifest(cfg ManifestConfig) *Manifest {
	return &Manifest{
		SchemaVersion:        SchemaVersion,
		Targets:              sortedCopy(cfg.Targets),
		MaxDepth:             cfg.MaxDepth,
		CrawlDurationSeconds: cfg.CrawlDurationSeconds,
		Exclude:              sortedCopy(cfg.Exclude),
		Scope:                sortedCopy(cfg.Scope),
		OutOfScope:           sortedCopy(cfg.OutOfScope),
		FieldScope:           cfg.FieldScope,
		NoScope:              cfg.NoScope,
		ExtensionsMatch:      sortedCopy(cfg.ExtensionsMatch),
		ExtensionFilter:      sortedCopy(cfg.ExtensionFilter),
		NoDefaultExtFilter:   cfg.NoDefaultExtFilter,
		IgnoreQueryParams:    cfg.IgnoreQueryParams,
		MaxDomainPages:       cfg.MaxDomainPages,
		PathClimb:            cfg.PathClimb,
	}
}

func sortedCopy(vals []string) []string {
	if len(vals) == 0 {
		return nil
	}
	out := append([]string(nil), vals...)
	// Trim whitespace defensively; drop empties.
	filtered := out[:0]
	for _, v := range out {
		if v = strings.TrimSpace(v); v != "" {
			filtered = append(filtered, v)
		}
	}
	sort.Strings(filtered)
	return filtered
}
