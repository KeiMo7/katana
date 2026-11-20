package runner

import (
	"fmt"
	"strings"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/katana/pkg/endpointdiff"
)

// manifestConfig adapts the CLI options into the diff package's flat manifest
// configuration. Only scope-defining options are included: purely cosmetic or
// performance-related flags cannot change which endpoints are observable.
func (r *Runner) manifestConfig() endpointdiff.ManifestConfig {
	o := r.options
	return endpointdiff.ManifestConfig{
		// parseInputs is the same normalization applied to actual crawl
		// inputs, so the manifest records exactly what was crawled.
		Targets:              r.parseInputs(),
		MaxDepth:             o.MaxDepth,
		CrawlDurationSeconds: int64(o.CrawlDuration.Seconds()),
		Exclude:              append([]string(nil), o.Exclude...),
		Scope:                append([]string(nil), o.Scope...),
		OutOfScope:           append([]string(nil), o.OutOfScope...),
		FieldScope:           o.FieldScope,
		NoScope:              o.NoScope,
		ExtensionsMatch:      append([]string(nil), o.ExtensionsMatch...),
		ExtensionFilter:      append([]string(nil), o.ExtensionFilter...),
		NoDefaultExtFilter:   o.NoDefaultExtFilter,
		IgnoreQueryParams:    o.IgnoreQueryParams,
		MaxDomainPages:       o.MaxDomainPages,
		PathClimb:            o.PathClimb,
	}
}

// WriteCrawlManifest writes the scope snapshot beside the current inventory so
// a future run can diff against it. Errors are returned to the caller: a
// silently missing manifest would later force the "cannot distinguish
// removal from uncovered" failure path.
func (r *Runner) WriteCrawlManifest() error {
	if r.options.OutputFile == "" {
		return fmt.Errorf("-crawl-manifest requires -o to locate the inventory file")
	}
	manifestPath := endpointdiff.ManifestPathFor(r.options.OutputFile)
	manifest := endpointdiff.NewManifest(r.manifestConfig())
	if err := endpointdiff.WriteManifest(manifestPath, manifest); err != nil {
		return fmt.Errorf("writing crawl manifest: %w", err)
	}
	gologger.Info().Msgf("Crawl manifest written to %s", manifestPath)
	return nil
}

// RunEndpointDiff compares the current inventory against the configured
// baseline and writes the change report. It runs after the crawler and its
// writer are closed, so both jsonl files are fully flushed on disk.
func (r *Runner) RunEndpointDiff() error {
	o := r.options
	if o.OutputFile == "" {
		return fmt.Errorf("-diff requires -o to locate the current endpoint inventory")
	}
	reportPath := o.DiffReport
	if reportPath == "" {
		reportPath = o.OutputFile + ".diff.json"
	}
	report, err := endpointdiff.RunFromFiles(endpointdiff.RunConfig{
		BaselineFile:         o.DiffBaseline,
		CurrentFile:          o.OutputFile,
		ReportFile:           reportPath,
		AllowMissingManifest: o.DiffAllowMissingManifest,
	})
	if err != nil {
		return err
	}
	logDiffSummary(report, reportPath)
	return nil
}

func logDiffSummary(r *endpointdiff.Report, reportPath string) {
	gologger.Info().Msgf(
		"Endpoint diff written to %s — added: %d, removed: %d, not covered: %d, persisted: %d (param changes: %d, source changes: %d, routes moved: %d)",
		reportPath,
		r.Summary.Added,
		r.Summary.Removed,
		r.Summary.NotCovered,
		r.Summary.Persisted,
		r.Summary.ParamChanged,
		r.Summary.SourceChanged,
		r.Summary.Rewired,
	)
	if r.ScopeNotice != nil {
		gologger.Warning().Msgf("Crawl scope mismatch: %s", strings.Join(r.ScopeNotice.DifferingFields, ", "))
	}
	if r.StructuralChange.Detected {
		gologger.Warning().Msg(r.StructuralChange.Message)
	}
}
