package endpointdiff

import (
	"fmt"
)

// RunConfig is the file-level input for a diff run.
type RunConfig struct {
	// BaselineFile / CurrentFile are the two jsonl inventories (baseline =
	// older week, current = newer week).
	BaselineFile string
	CurrentFile  string
	// ReportFile receives the atomic JSON report.
	ReportFile string
	// BaselineManifestFile / CurrentManifestFile override the default sibling
	// manifest resolution when set (empty means derive via ManifestPathFor).
	BaselineManifestFile string
	CurrentManifestFile  string
	// AllowMissingManifest downgrades a missing baseline manifest from an
	// error to a scope warning.
	AllowMissingManifest bool
}

// RunFromFiles streams both inventories, resolves their manifests, computes
// the diff and writes the report atomically. It returns the in-memory report
// as well so callers can print a summary without re-reading the file.
func RunFromFiles(cfg RunConfig) (*Report, error) {
	if cfg.BaselineFile == "" || cfg.CurrentFile == "" {
		return nil, fmt.Errorf("endpointdiff: baseline and current inventory files are required")
	}
	baseline, err := ReadInventoryFile(cfg.BaselineFile)
	if err != nil {
		return nil, fmt.Errorf("reading baseline inventory: %w", err)
	}
	current, err := ReadInventoryFile(cfg.CurrentFile)
	if err != nil {
		return nil, fmt.Errorf("reading current inventory: %w", err)
	}

	baselineManifestPath := cfg.BaselineManifestFile
	if baselineManifestPath == "" {
		baselineManifestPath = ManifestPathFor(cfg.BaselineFile)
	}
	currentManifestPath := cfg.CurrentManifestFile
	if currentManifestPath == "" {
		currentManifestPath = ManifestPathFor(cfg.CurrentFile)
	}
	baselineManifest, baselineExists, err := ReadManifest(baselineManifestPath)
	if err != nil {
		return nil, err
	}
	currentManifest, currentExists, err := ReadManifest(currentManifestPath)
	if err != nil {
		return nil, err
	}

	report, err := Compare(baseline, current, Options{
		BaselineManifest:        baselineManifest,
		CurrentManifest:         currentManifest,
		BaselineManifestMissing: !baselineExists,
		CurrentManifestMissing:  !currentExists,
		AllowMissingManifest:    cfg.AllowMissingManifest,
		BaselineFile:            cfg.BaselineFile,
		CurrentFile:             cfg.CurrentFile,
	})
	if err != nil {
		return nil, err
	}
	if cfg.ReportFile != "" {
		if err := WriteReport(cfg.ReportFile, report); err != nil {
			return nil, err
		}
	}
	return report, nil
}
