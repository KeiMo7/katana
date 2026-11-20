package standard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/projectdiscovery/fastdialer/fastdialer"
	"github.com/projectdiscovery/katana/pkg/engine/parser"
	"github.com/projectdiscovery/katana/pkg/navigation"
	"github.com/projectdiscovery/katana/pkg/output"
	"github.com/projectdiscovery/katana/pkg/state"
	"github.com/projectdiscovery/katana/pkg/types"
	"github.com/projectdiscovery/katana/pkg/utils/extensions"
	"github.com/projectdiscovery/katana/pkg/utils/filters"
	"github.com/projectdiscovery/katana/pkg/utils/scope"
	"github.com/stretchr/testify/require"
)

type mockWriter struct {
	mu      sync.Mutex
	results []*output.Result
}

func (w *mockWriter) Close() error { return nil }
func (w *mockWriter) Write(r *output.Result) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.results = append(w.results, r)
	return nil
}
func (w *mockWriter) WriteErr(_ *output.Error) error { return nil }
func (w *mockWriter) GetResultCount() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.results))
}

func (w *mockWriter) urls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.results))
	for _, r := range w.results {
		out = append(out, r.Request.URL)
	}
	return out
}

// graphServer serves a small deterministic site:
//
//	/       -> /a /b
//	/a      -> /c /d
//	/b      -> /c /a   (duplicates and a cycle)
//	/c      -> /e
//	/extra  -> /f
//	/dup    -> (counted; linked with several spellings from /spellings)
//	/spellings -> //dup, /dup#frag, /dup#other
func graphServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var dupHits atomic.Int64
	mux := http.NewServeMux()
	page := func(links ...string) string {
		var b strings.Builder
		b.WriteString("<html><body>")
		for _, l := range links {
			b.WriteString(fmt.Sprintf(`<a href="%s">x</a>`, l))
		}
		b.WriteString("</body></html>")
		return b.String()
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// every page embeds its own path so the unique-content filter does
		// not collapse distinct pages into one
		body := func(links ...string) string {
			return fmt.Sprintf("<!-- %s -->", r.URL.Path) + page(links...)
		}
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, body("/a", "/b"))
		case "/a":
			fmt.Fprint(w, body("/c", "/d"))
		case "/b":
			fmt.Fprint(w, body("/c", "/a"))
		case "/c":
			fmt.Fprint(w, body("/e"))
		case "/d", "/e", "/f":
			fmt.Fprint(w, body())
		case "/extra":
			fmt.Fprint(w, body("/f"))
		case "/spellings":
			// two absolute spellings of the same page: one with a doubled
			// slash in the path. (Fragments never reach the fetch layer at
			// all - the engine strips them when resolving links.)
			fmt.Fprint(w, body("http://"+r.Host+"//dup", "http://"+r.Host+"/dup"))
		case "/dup", "//dup":
			dupHits.Add(1)
			fmt.Fprint(w, body())
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &dupHits
}

func newResumeTestCrawler(t *testing.T, opts *types.Options, writer *mockWriter, tracker *state.Tracker) *Crawler {
	t.Helper()
	if opts.Strategy == "" {
		opts.Strategy = "depth-first"
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = 4
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5
	}
	if opts.BodyReadSize == 0 {
		opts.BodyReadSize = 4 * 1024 * 1024
	}
	scopeManager, err := scope.NewManager(nil, nil, "", true)
	require.NoError(t, err)
	itemFilter, err := filters.NewSimple()
	require.NoError(t, err)
	responseParser := parser.NewResponseParser()
	responseParser.InitWithOptions(&parser.Options{})
	dialer, err := fastdialer.NewDialer(fastdialer.DefaultOptions)
	require.NoError(t, err)

	crawlerOptions := &types.CrawlerOptions{
		Options:             opts,
		OutputWriter:        writer,
		UniqueFilter:        itemFilter,
		ScopeManager:        scopeManager,
		ExtensionsValidator: extensions.NewValidator(nil, nil, true),
		Parser:              responseParser,
		Dialer:              dialer,
		StateTracker:        tracker,
	}
	crawler, err := New(crawlerOptions)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = crawler.Close()
		_ = crawlerOptions.Close()
	})
	return crawler
}

func urlSet(urls []string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, u := range urls {
		set[u] = struct{}{}
	}
	return set
}

// TestResumeFinalSetMatchesSingleRun is the core acceptance test: a crawl
// interrupted halfway and then resumed must produce exactly the same URL
// set as a single uninterrupted crawl, and no page may be fetched twice.
func TestResumeFinalSetMatchesSingleRun(t *testing.T) {
	srv, _ := graphServer(t)
	stateDir := t.TempDir()

	// baseline: one uninterrupted crawl, no state tracking
	baselineWriter := &mockWriter{}
	baseline := newResumeTestCrawler(t, &types.Options{MaxDepth: 5}, baselineWriter, nil)
	require.NoError(t, baseline.Crawl(srv.URL))
	baselineSet := urlSet(baselineWriter.urls())
	require.NotEmpty(t, baselineSet)

	// run 1: interrupted after a couple of results
	ctx, cancel := context.WithCancel(context.Background())
	var seen atomic.Int64
	run1Writer := &mockWriter{}
	run1Opts := &types.Options{
		MaxDepth: 5,
		Context:  ctx,
		OnResult: func(output.Result) {
			if seen.Add(1) >= 2 {
				cancel()
			}
		},
	}
	tracker1, err := state.Open(stateDir, []string{srv.URL})
	require.NoError(t, err)
	run1 := newResumeTestCrawler(t, run1Opts, run1Writer, tracker1)
	_ = run1.Crawl(srv.URL) // context cancellation surfaces as an error
	require.NoError(t, tracker1.Close())
	require.NotEmpty(t, run1Writer.urls(), "first run should have produced some results before interruption")

	// run 2: resumed from the state directory to completion
	run2Writer := &mockWriter{}
	tracker2, err := state.Open(stateDir, []string{srv.URL})
	require.NoError(t, err)
	require.True(t, tracker2.Resumed())
	run2 := newResumeTestCrawler(t, &types.Options{MaxDepth: 5}, run2Writer, tracker2)
	require.NoError(t, run2.Crawl(srv.URL))

	reused, fetched, failed := tracker2.Stats()
	require.NoError(t, tracker2.Close())

	// the union of both runs equals the single-run baseline, exactly
	resumedSet := urlSet(append(run1Writer.urls(), run2Writer.urls()...))
	require.Equal(t, baselineSet, resumedSet)

	// no URL was *successfully* crawled in both runs; a URL may appear in
	// run 1 with a fetch error (the interruption itself) and then be
	// legitimately retried by run 2
	run1Set := urlSet(run1Writer.urls())
	run1Errors := map[string]bool{}
	for _, r := range run1Writer.results {
		run1Errors[r.Request.URL] = r.Error != ""
	}
	for _, u := range run2Writer.urls() {
		if _, dup := run1Set[u]; dup {
			require.True(t, run1Errors[u], "URL %s was successfully crawled again after resume", u)
		}
	}

	// progress accounting: part of the work was reused, part fetched now
	require.Positive(t, reused, "some URLs should have been reused from the checkpoint")
	require.Equal(t, len(run2Writer.urls()), fetched)
	require.Zero(t, failed)
}

// TestResumeSeedListChanges covers entry points changing between runs:
// new entry points are picked up, a replaced list is refused.
func TestResumeSeedListChanges(t *testing.T) {
	srv, _ := graphServer(t)
	stateDir := t.TempDir()

	// interrupted first run with a single entry point
	ctx, cancel := context.WithCancel(context.Background())
	var seen atomic.Int64
	run1Writer := &mockWriter{}
	tracker1, err := state.Open(stateDir, []string{srv.URL})
	require.NoError(t, err)
	run1 := newResumeTestCrawler(t, &types.Options{
		MaxDepth: 5,
		Context:  ctx,
		OnResult: func(output.Result) {
			if seen.Add(1) >= 2 {
				cancel()
			}
		},
	}, run1Writer, tracker1)
	_ = run1.Crawl(srv.URL)
	require.NoError(t, tracker1.Close())

	// resume with an additional entry point: /extra (and through it /f)
	// must be crawled as well
	run2Writer := &mockWriter{}
	tracker2, err := state.Open(stateDir, []string{srv.URL, srv.URL + "/extra"})
	require.NoError(t, err)
	run2 := newResumeTestCrawler(t, &types.Options{MaxDepth: 5}, run2Writer, tracker2)
	require.NoError(t, run2.Crawl(srv.URL))
	require.NoError(t, tracker2.Close())

	resumedSet := urlSet(append(run1Writer.urls(), run2Writer.urls()...))
	require.Contains(t, resumedSet, srv.URL+"/extra")
	require.Contains(t, resumedSet, srv.URL+"/f")

	// baseline for the extended entry list, crawled in one go
	baselineWriter := &mockWriter{}
	baseline := newResumeTestCrawler(t, &types.Options{MaxDepth: 5}, baselineWriter, nil)
	require.NoError(t, baseline.Crawl(srv.URL))
	require.NoError(t, baseline.Crawl(srv.URL+"/extra"))
	require.Equal(t, urlSet(baselineWriter.urls()), resumedSet)

	// a completely different entry point list is rejected
	_, err = state.Open(stateDir, []string{"http://example.invalid/"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to resume")
}

// TestResumeCorruptedState verifies a corrupted state file is a hard,
// actionable error instead of a silent fresh start.
func TestResumeCorruptedState(t *testing.T) {
	srv, _ := graphServer(t)
	stateDir := t.TempDir()

	tracker, err := state.Open(stateDir, []string{srv.URL})
	require.NoError(t, err)
	tracker.MarkPending(navigationReq(srv.URL + "/a"))
	require.NoError(t, tracker.Close())

	require.NoError(t, corruptFile(stateDir))

	_, err = state.Open(stateDir, []string{srv.URL})
	require.Error(t, err)
	require.Contains(t, err.Error(), "corrupted")
	require.Contains(t, err.Error(), "fresh crawl")
}

// TestNormalizedURLEnqueuedOnce verifies that different spellings of the
// same page collapse to a single fetch when state tracking is enabled.
func TestNormalizedURLEnqueuedOnce(t *testing.T) {
	srv, dupHits := graphServer(t)

	// without state tracking the two spellings are two different URLs
	plainWriter := &mockWriter{}
	plain := newResumeTestCrawler(t, &types.Options{MaxDepth: 2}, plainWriter, nil)
	require.NoError(t, plain.Crawl(srv.URL+"/spellings"))
	require.Equal(t, int64(2), dupHits.Load(), "without -crawl-state-dir each spelling is fetched on its own")

	// with state tracking they normalize to one identity and are fetched once
	dupHits.Store(0)
	trackedWriter := &mockWriter{}
	tracker, err := state.Open(t.TempDir(), []string{srv.URL + "/spellings"})
	require.NoError(t, err)
	tracked := newResumeTestCrawler(t, &types.Options{MaxDepth: 2}, trackedWriter, tracker)
	require.NoError(t, tracked.Crawl(srv.URL+"/spellings"))
	require.NoError(t, tracker.Close())
	require.Equal(t, int64(1), dupHits.Load(), "normalized spellings of one page must be enqueued only once")
}

func navigationReq(url string) *navigation.Request {
	return &navigation.Request{Method: http.MethodGet, URL: url}
}

func corruptFile(dir string) error {
	return os.WriteFile(filepath.Join(dir, state.SnapshotFile), []byte("{broken"), 0o644)
}
