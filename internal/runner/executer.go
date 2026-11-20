package runner

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/katana/pkg/state"
	"github.com/projectdiscovery/utils/errkit"
	urlutil "github.com/projectdiscovery/utils/url"
	"github.com/remeh/sizedwaitgroup"
)

// ExecuteCrawling executes the crawling main loop
func (r *Runner) ExecuteCrawling() error {
	if r.crawler == nil {
		return errkit.New("crawler is not initialized")
	}
	inputs := r.parseInputs()
	if len(inputs) == 0 {
		return errkit.New("no input provided for crawling")
	}

	for _, input := range inputs {
		_ = r.state.InFlightUrls.Set(addSchemeIfNotExists(input), struct{}{})
	}

	// Enable resumable crawling when a state directory is configured. Any
	// problem with the state (unwritable directory, corrupted file, seed
	// list mismatch) aborts the run before a single request is sent.
	if r.options.CrawlStateDir != "" {
		seeds := make([]string, 0, len(inputs))
		for _, input := range inputs {
			seeds = append(seeds, addSchemeIfNotExists(input))
		}
		tracker, err := state.Open(r.options.CrawlStateDir, seeds)
		if err != nil {
			return err
		}
		r.crawlerOptions.StateTracker = tracker
		if tracker.Resumed() {
			progress := tracker.Progress()
			gologger.Info().Msgf("Resuming crawl from %s: %d done, %d pending, %d failed (will be retried)",
				r.options.CrawlStateDir, progress.Done, progress.Pending, progress.Failed)
		}
	}

	// Track crawl timing
	startTime := time.Now()

	defer func() {
		if err := r.crawler.Close(); err != nil {
			gologger.Error().Msgf("Error closing crawler: %v\n", err)
		}
	}()

	var done atomic.Int64
	if r.options.ShowStatistics {
		defer r.startStats(startTime, &done, len(inputs))()
	}

	wg := sizedwaitgroup.New(r.options.Parallelism)
	for _, input := range inputs {
		if !r.networkpolicy.Validate(input) {
			gologger.Info().Msgf("Skipping excluded host %s", input)
			done.Add(1)
			continue
		}
		wg.Add()
		input = addSchemeIfNotExists(input)
		go func(input string) {
			defer wg.Done()
			defer done.Add(1)

			if err := r.crawler.Crawl(input); err != nil {
				gologger.Warning().Msgf("Could not crawl %s: %s", input, err)
			}
			r.state.InFlightUrls.Delete(input)
		}(input)
	}
	wg.Wait()

	// Report how much of the crawl was reused from the checkpoint versus
	// fetched during this run
	if tracker := r.crawlerOptions.StateTracker; tracker != nil {
		reused, fetched, failed := tracker.Stats()
		gologger.Info().Msgf("Crawl state: %d URLs reused from checkpoint, %d fetched this run, %d failed (recorded for retry)", reused, fetched, failed)
	}

	// Show completion message with stats
	r.showCompletionStats(startTime)

	return nil
}

// scheme less urls are skipped and are required for headless mode and other purposes
// this method adds scheme if given input does not have any
func addSchemeIfNotExists(inputURL string) string {
	if strings.HasPrefix(inputURL, urlutil.HTTP) || strings.HasPrefix(inputURL, urlutil.HTTPS) {
		return inputURL
	}
	parsed, err := urlutil.Parse(inputURL)
	if err != nil {
		gologger.Warning().Msgf("input %v is not a valid url got %v", inputURL, err)
		return inputURL
	}
	if parsed.Port() != "" && (parsed.Port() == "80" || parsed.Port() == "8080") {
		return urlutil.HTTP + urlutil.SchemeSeparator + inputURL
	} else {
		return urlutil.HTTPS + urlutil.SchemeSeparator + inputURL
	}
}

// showCompletionStats shows the final crawl completion message with timing and stats
func (r *Runner) showCompletionStats(startTime time.Time) {
	// Calculate elapsed time
	elapsed := time.Since(startTime)

	// Get total endpoints discovered
	endpointCount := r.crawlerOptions.OutputWriter.GetResultCount()

	// Format elapsed time in human-readable format
	timeStr := formatDuration(elapsed)

	// Show content similarity stats first if enabled
	if r.crawlerOptions.ContentSimilarity != nil {
		st := r.crawlerOptions.ContentSimilarity.Stats()
		if st.Processed > 0 {
			filterRate := float64(st.Filtered) / float64(st.Processed) * 100
			gologger.Info().Msgf("Content similarity (%s): %d processed, %d accepted, %d filtered - %.1f%% filter rate",
				r.crawlerOptions.ContentSimilarity.Mode(), st.Processed, st.Accepted, st.Filtered, filterRate)
		}
	}

	// Show clean completion message
	gologger.Info().Msgf("Crawl completed in %s. %d endpoints found.", timeStr, endpointCount)
}

// formatDuration formats a duration in human-readable format
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Nanoseconds()/1e6)
	}

	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	} else if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	} else {
		return fmt.Sprintf("%ds", seconds)
	}
}
