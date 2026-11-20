package state

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/projectdiscovery/katana/pkg/navigation"
	"github.com/stretchr/testify/require"
)

func req(url string, depth int) *navigation.Request {
	return &navigation.Request{Method: http.MethodGet, URL: url, Depth: depth}
}

func TestStateFreshRunThenResume(t *testing.T) {
	dir := t.TempDir()

	tr, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err)
	require.False(t, tr.Resumed())

	tr.MarkPending(req("http://example.com/a", 1))
	tr.MarkPending(req("http://example.com/b", 1))
	tr.MarkDone(req("http://example.com/a", 1))
	tr.MarkFailed(req("http://example.com/c", 1), errors.New("read: connection reset by peer"))
	require.NoError(t, tr.Close())

	tr2, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err)
	require.True(t, tr2.Resumed())

	// /a is done and must not be enqueued again; /b is still pending
	require.True(t, tr2.IsKnown("http://example.com/a"))
	require.True(t, tr2.IsKnown("http://example.com/b"))
	require.False(t, tr2.IsKnown("http://example.com/never-seen"))

	// normalization: a different spelling of a done URL is still known
	require.True(t, tr2.IsKnown("HTTP://EXAMPLE.COM//a#frag"))

	// the failed URL is queued for retry and its failure kind is traceable
	require.True(t, tr2.IsKnown("http://example.com/c"))
	failed := tr2.Failed()
	require.Equal(t, "connection_reset", failed["http://example.com/c"].Kind)
	require.Equal(t, 1, failed["http://example.com/c"].Count)

	// pending entries restore in discovery order, carrying depth
	restored := tr2.RestorePending("example.com")
	var urls []string
	for _, r := range restored {
		urls = append(urls, r.URL)
	}
	require.Contains(t, urls, "http://example.com/b")
	require.Contains(t, urls, "http://example.com/c")
	require.NotContains(t, urls, "http://example.com/a")
	// restore happens once per host
	require.Empty(t, tr2.RestorePending("example.com"))

	reused, _, failedCount := tr2.Stats()
	require.Equal(t, 1, reused)
	require.Equal(t, 1, failedCount)
	require.NoError(t, tr2.Close())
}

func TestStateSeedListChanges(t *testing.T) {
	dir := t.TempDir()

	tr, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err)
	tr.MarkDone(req("http://example.com/a", 1))
	require.NoError(t, tr.Close())

	t.Run("additional seeds are accepted and queued", func(t *testing.T) {
		tr2, err := Open(dir, []string{"http://example.com/", "http://example.com/new-entry"})
		require.NoError(t, err)
		require.True(t, tr2.IsKnown("http://example.com/new-entry"))
		require.NoError(t, tr2.Close())
	})

	t.Run("replacing the seed list is refused", func(t *testing.T) {
		_, err := Open(dir, []string{"http://other.example/"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "refusing to resume")
		require.Contains(t, err.Error(), "fresh crawl")
	})
}

func TestStateCorruptedSnapshot(t *testing.T) {
	dir := t.TempDir()

	tr, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err)
	tr.MarkDone(req("http://example.com/a", 1))
	require.NoError(t, tr.Close())

	require.NoError(t, os.WriteFile(filepath.Join(dir, SnapshotFile), []byte("{not json"), 0o644))

	_, err = Open(dir, []string{"http://example.com/"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "corrupted")
	require.Contains(t, err.Error(), "fresh crawl")
}

func TestStateCorruptedJournalMiddle(t *testing.T) {
	dir := t.TempDir()

	tr, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err)
	tr.MarkDone(req("http://example.com/a", 1))
	require.NoError(t, tr.Close())

	// a valid terminated line that is not JSON in the middle of the journal
	f, err := os.OpenFile(filepath.Join(dir, JournalFile), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("garbage-line\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = Open(dir, []string{"http://example.com/"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "corrupted")
}

func TestStateJournalTornTailTolerated(t *testing.T) {
	dir := t.TempDir()

	tr, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err)
	tr.MarkDone(req("http://example.com/a", 1))
	require.NoError(t, tr.Close())

	// simulate a kill in the middle of a journal write: partial final line
	f, err := os.OpenFile(filepath.Join(dir, JournalFile), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"op":"done","url":"http://exa`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	tr2, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err, "a torn final journal line must be truncated, not fatal")
	require.True(t, tr2.IsKnown("http://example.com/a"))
	require.NoError(t, tr2.Close())
}

func TestStateUnwritableDirectory(t *testing.T) {
	// a regular file cannot be used as a state directory
	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	_, err := Open(file, []string{"http://example.com/"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not writable")
}

func TestStateProgress(t *testing.T) {
	dir := t.TempDir()

	tr, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err)
	tr.MarkPending(req("http://example.com/a", 1))
	tr.MarkInFlight(req("http://example.com/a", 1))
	tr.MarkDone(req("http://example.com/b", 1))

	p := tr.Progress()
	require.Equal(t, 1, p.Done)
	require.Equal(t, 2, p.Pending) // seed + /a
	require.Equal(t, []string{"http://example.com/a"}, p.InFlight)
	require.NoError(t, tr.Close())
}

func TestStateSnapshotIsAtomicAndComplete(t *testing.T) {
	dir := t.TempDir()

	tr, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err)
	for _, u := range []string{"http://example.com/1", "http://example.com/2", "http://example.com/3"} {
		tr.MarkPending(req(u, 1))
	}
	tr.MarkDone(req("http://example.com/1", 1))
	require.NoError(t, tr.Close())

	// no temporary snapshot files may linger after a clean close
	matches, err := filepath.Glob(filepath.Join(dir, ".crawl-state-*.tmp"))
	require.NoError(t, err)
	require.Empty(t, matches)

	// the snapshot alone (journal discarded) still holds the full state
	require.NoError(t, os.Remove(filepath.Join(dir, JournalFile)))
	tr2, err := Open(dir, []string{"http://example.com/"})
	require.NoError(t, err)
	require.True(t, tr2.IsKnown("http://example.com/1"))
	require.True(t, tr2.IsKnown("http://example.com/2"))
	require.True(t, tr2.IsKnown("http://example.com/3"))
	require.NoError(t, tr2.Close())
}
