// Package state implements durable, resumable crawl progress.
//
// A Tracker records which URLs have been crawled (done), which are waiting
// to be crawled (pending, in discovery order) and which failed last time
// (with the failure kind, so a later run can retry and the user can
// inspect why). Progress is persisted in a state directory as:
//
//	crawl-state.json     - atomic snapshot (tmp file + rename + dir fsync)
//	crawl-state.journal  - append-only event log replayed on top of the snapshot
//
// Every journal append is fsynced, so a kill at any moment loses nothing
// that was acknowledged; a torn final line (partial write) is truncated on
// load. The snapshot is rewritten atomically every compactEvery events and
// on Close, so the journal stays bounded.
package state

import (
	"bufio"
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/projectdiscovery/katana/pkg/navigation"
	"github.com/projectdiscovery/katana/pkg/utils"
)

const (
	// SnapshotFile is the atomically-replaced full-state snapshot.
	SnapshotFile = "crawl-state.json"
	// JournalFile is the append-only event log applied after the snapshot.
	JournalFile = "crawl-state.journal"

	snapshotVersion = 1
	compactEvery    = 500
)

// PendingEntry is a URL that was accepted for crawling but not finished yet.
type PendingEntry struct {
	Method       string `json:"method,omitempty"`
	URL          string `json:"url"`
	Depth        int    `json:"depth"`
	Source       string `json:"source,omitempty"`
	Tag          string `json:"tag,omitempty"`
	RootHostname string `json:"root_hostname,omitempty"`
	Seed         bool   `json:"seed,omitempty"`
}

// FailRecord remembers the last failure for a URL so a resumed run can
// retry it and the user can trace what went wrong previously.
type FailRecord struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Count   int    `json:"count"`
}

type snapshot struct {
	Version int                     `json:"version"`
	Seeds   []string                `json:"seeds"`
	Done    []string                `json:"done"`
	Failed  map[string]FailRecord   `json:"failed,omitempty"`
	Pending map[string]PendingEntry `json:"pending,omitempty"`
}

type journalEvent struct {
	Op      string        `json:"op"` // seed | pending | done | failed
	Entry   *PendingEntry `json:"entry,omitempty"`
	URL     string        `json:"url,omitempty"` // normalized identity
	Kind    string        `json:"kind,omitempty"`
	Message string        `json:"message,omitempty"`
}

// Progress is a point-in-time readable view of the crawl.
type Progress struct {
	Done     int      `json:"done"`
	Pending  int      `json:"pending"`
	Failed   int      `json:"failed"`
	InFlight []string `json:"in_flight"`
}

// Tracker tracks crawl progress durably. The zero value is unusable;
// create one with Open. All methods are safe for concurrent use.
type Tracker struct {
	mu       sync.Mutex
	dir      string
	journal  *os.File
	journalW *bufio.Writer

	seeds   map[string]struct{}
	seedLst []string

	done   map[string]struct{}
	failed map[string]FailRecord

	// pending is kept as map + list so discovery order survives a restart
	pendingIdx map[string]*list.Element
	pendingLst *list.List // of PendingEntry

	inflight map[string]struct{}

	restoredHosts map[string]bool

	eventsSinceCompact int
	reused             int // done entries loaded from a previous run
	fetchedThisRun     int
	resumed            bool
	closed             bool
}

// Open opens (or creates) the crawl state in dir and validates it against
// the given seed URLs (the crawl entry points, scheme included).
//
// Behaviour:
//   - dir missing: created; a fresh state is started.
//   - dir not writable: error, before any crawling happens.
//   - state present and every recorded seed is still in seeds: resume.
//     Extra seeds are allowed and are appended to the pending queue.
//   - state present but a recorded seed is missing from seeds: error,
//     the run refuses to mix results from a different target list.
//   - snapshot corrupted: error naming the file and the remedy.
func Open(dir string, seeds []string) (*Tracker, error) {
	t := &Tracker{
		dir:           dir,
		seeds:         map[string]struct{}{},
		done:          map[string]struct{}{},
		failed:        map[string]FailRecord{},
		pendingIdx:    map[string]*list.Element{},
		pendingLst:    list.New(),
		inflight:      map[string]struct{}{},
		restoredHosts: map[string]bool{},
	}

	normSeeds := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		norm, err := utils.NormalizeURL(seed)
		if err != nil {
			return nil, fmt.Errorf("crawl state: could not normalize seed %q: %w", seed, err)
		}
		normSeeds = append(normSeeds, norm)
	}

	if err := probeWritable(dir); err != nil {
		return nil, err
	}

	snapPath := filepath.Join(dir, SnapshotFile)
	if _, err := os.Stat(snapPath); err == nil {
		if err := t.loadSnapshot(snapPath); err != nil {
			return nil, err
		}
		if err := t.replayJournal(filepath.Join(dir, JournalFile)); err != nil {
			return nil, err
		}
		t.resumed = true
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("crawl state: could not stat %s: %w", snapPath, err)
	}

	// Every recorded seed must still be an entry point; adding new entry
	// points is fine, replacing the list is not.
	var missing []string
	for _, s := range t.seedLst {
		if !containsString(normSeeds, s) {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("crawl state in %s was recorded for seed(s) %v which are not in the current input; "+
			"refusing to resume to avoid mixing results from different targets. "+
			"Re-run with the original entry points (new ones may be added), or delete %s to start a fresh crawl",
			dir, missing, dir)
	}

	journal, err := os.OpenFile(filepath.Join(dir, JournalFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("crawl state: could not open journal in %s: %w", dir, err)
	}
	t.journal = journal
	t.journalW = bufio.NewWriter(journal)

	// Register seeds (new and, on a fresh state, all of them) as pending
	// entry points. Seeds already done or pending are skipped.
	for i, seed := range seeds {
		norm := normSeeds[i]
		if _, ok := t.seeds[norm]; !ok {
			t.seeds[norm] = struct{}{}
			t.seedLst = append(t.seedLst, norm)
		}
		entry := PendingEntry{Method: http.MethodGet, URL: seed, Depth: 0, Seed: true}
		if parsed, err := url.Parse(seed); err == nil {
			entry.RootHostname = strings.ToLower(parsed.Hostname())
		}
		t.addPendingLocked(norm, entry)
	}

	// Failed URLs from the previous run are retried: move them back to the
	// pending queue while keeping the failure record for traceability.
	for norm := range t.failed {
		if _, ok := t.pendingIdx[norm]; ok {
			continue
		}
		t.addPendingLocked(norm, PendingEntry{Method: http.MethodGet, URL: norm, Depth: 0})
	}

	if !t.resumed {
		// persist the initial state so a kill before the first completion
		// still leaves a valid, resumable state behind
		if err := t.compactLocked(); err != nil {
			_ = journal.Close()
			return nil, err
		}
	}
	return t, nil
}

// Resumed reports whether this tracker continues a previous run.
func (t *Tracker) Resumed() bool { return t.resumed }

// IsKnown reports whether rawURL (any spelling) is already completed or
// already waiting in the pending queue, and therefore must not be
// enqueued again.
func (t *Tracker) IsKnown(rawURL string) bool {
	norm, err := utils.NormalizeURL(rawURL)
	if err != nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.done[norm]; ok {
		return true
	}
	_, ok := t.pendingIdx[norm]
	return ok
}

// MarkPending records a newly enqueued request. It is a no-op for URLs
// already done or pending.
func (t *Tracker) MarkPending(req *navigation.Request) {
	norm, err := utils.NormalizeURL(req.URL)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.addPendingLocked(norm, PendingEntry{
		Method:       req.Method,
		URL:          req.URL,
		Depth:        req.Depth,
		Source:       req.Source,
		Tag:          req.Tag,
		RootHostname: req.RootHostname,
	})
}

// MarkInFlight notes that req is currently being fetched (for progress).
func (t *Tracker) MarkInFlight(req *navigation.Request) {
	norm, err := utils.NormalizeURL(req.URL)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight[norm] = struct{}{}
}

// MarkDone records req as successfully crawled.
func (t *Tracker) MarkDone(req *navigation.Request) {
	t.settle(req, nil, true)
}

// MarkSkipped records req as settled without having been fetched (for
// example dropped by path or scope filters at pop time), so a resumed run
// does not resurrect it forever.
func (t *Tracker) MarkSkipped(req *navigation.Request) {
	t.settle(req, nil, false)
}

// MarkAborted clears the in-flight mark of a request whose fetch was
// interrupted by shutdown. The URL stays pending so the next run retries
// it, but no failure is recorded: being interrupted is not a failure.
func (t *Tracker) MarkAborted(req *navigation.Request) {
	norm, err := utils.NormalizeURL(req.URL)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.inflight, norm)
}

// MarkFailed records req as failed and keeps the failure kind so the next
// run retries it and the user can inspect what happened.
func (t *Tracker) MarkFailed(req *navigation.Request, cause error) {
	t.settle(req, cause, false)
}

func (t *Tracker) settle(req *navigation.Request, cause error, fetched bool) {
	norm, err := utils.NormalizeURL(req.URL)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.inflight, norm)
	t.removePendingLocked(norm)
	if cause == nil {
		if _, ok := t.done[norm]; !ok {
			t.done[norm] = struct{}{}
			if fetched {
				t.fetchedThisRun++
			}
			t.appendEventLocked(journalEvent{Op: "done", URL: norm})
		}
		return
	}
	rec := t.failed[norm]
	rec.Kind = classifyError(cause)
	rec.Message = cause.Error()
	rec.Count++
	t.failed[norm] = rec
	t.appendEventLocked(journalEvent{Op: "failed", URL: norm, Kind: rec.Kind, Message: rec.Message})
}

// RestorePending returns the pending requests belonging to hostname
// (exactly once per host per run) so they can be pushed back onto the
// crawl queue. Entries without a recorded host go to the first session
// that asks.
func (t *Tracker) RestorePending(hostname string) []*navigation.Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.restoredHosts[hostname] {
		return nil
	}
	t.restoredHosts[hostname] = true

	var out []*navigation.Request
	for e := t.pendingLst.Front(); e != nil; e = e.Next() {
		entry := e.Value.(PendingEntry)
		if entry.RootHostname != "" && !strings.EqualFold(entry.RootHostname, hostname) {
			continue
		}
		out = append(out, &navigation.Request{
			Method:         orGet(entry.Method),
			URL:            entry.URL,
			Depth:          entry.Depth,
			Source:         entry.Source,
			Tag:            entry.Tag,
			RootHostname:   entry.RootHostname,
			SkipValidation: entry.Seed,
		})
	}
	return out
}

// Progress returns a readable snapshot of the crawl progress: how many
// URLs are done, how many remain, and which are being fetched right now.
func (t *Tracker) Progress() Progress {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := Progress{
		Done:    len(t.done),
		Pending: t.pendingLst.Len(),
		Failed:  len(t.failed),
	}
	for norm := range t.inflight {
		p.InFlight = append(p.InFlight, norm)
	}
	return p
}

// Stats reports how many completed URLs were reused from previous runs and
// how many were fetched during this run, plus the current failure count.
func (t *Tracker) Stats() (reused, fetchedThisRun, failed int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reused, t.fetchedThisRun, len(t.failed)
}

// Failed returns the recorded failures (normalized URL -> record).
func (t *Tracker) Failed() map[string]FailRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]FailRecord, len(t.failed))
	for k, v := range t.failed {
		out[k] = v
	}
	return out
}

// Close flushes a final atomic snapshot and closes the journal.
func (t *Tracker) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if err := t.flushLocked(); err != nil {
		_ = t.journal.Close()
		return err
	}
	if err := t.compactLocked(); err != nil {
		_ = t.journal.Close()
		return err
	}
	return t.journal.Close()
}

// --- internals (all *Locked require t.mu) ---

func (t *Tracker) addPendingLocked(norm string, entry PendingEntry) {
	if _, ok := t.done[norm]; ok {
		return
	}
	if _, ok := t.pendingIdx[norm]; ok {
		return
	}
	t.pendingIdx[norm] = t.pendingLst.PushBack(entry)
	t.appendEventLocked(journalEvent{Op: opFor(entry), Entry: &entry})
}

func opFor(entry PendingEntry) string {
	if entry.Seed {
		return "seed"
	}
	return "pending"
}

func (t *Tracker) removePendingLocked(norm string) {
	if el, ok := t.pendingIdx[norm]; ok {
		t.pendingLst.Remove(el)
		delete(t.pendingIdx, norm)
	}
}

func (t *Tracker) appendEventLocked(ev journalEvent) {
	if t.closed {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = t.journalW.Write(data)
	_ = t.journalW.WriteByte('\n')
	// flush + fsync every event: an acknowledged completion must survive a
	// kill -9. Journal writes are ordered, so one fsync also persists every
	// preceding event.
	if err := t.flushLocked(); err == nil {
		t.eventsSinceCompact++
		if t.eventsSinceCompact >= compactEvery {
			_ = t.compactLocked()
		}
	}
}

func (t *Tracker) flushLocked() error {
	if err := t.journalW.Flush(); err != nil {
		return err
	}
	return t.journal.Sync()
}

// compactLocked rewrites the full snapshot atomically and truncates the
// journal. The journal keeps accepting appends after truncation because it
// was opened with O_APPEND.
func (t *Tracker) compactLocked() error {
	snap := snapshot{
		Version: snapshotVersion,
		Seeds:   t.seedLst,
		Done:    make([]string, 0, len(t.done)),
		Failed:  t.failed,
		Pending: map[string]PendingEntry{},
	}
	for norm := range t.done {
		snap.Done = append(snap.Done, norm)
	}
	for e := t.pendingLst.Front(); e != nil; e = e.Next() {
		entry := e.Value.(PendingEntry)
		norm, err := utils.NormalizeURL(entry.URL)
		if err != nil {
			continue
		}
		snap.Pending[norm] = entry
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	snapPath := filepath.Join(t.dir, SnapshotFile)
	tmp, err := os.CreateTemp(t.dir, ".crawl-state-*.tmp")
	if err != nil {
		return fmt.Errorf("crawl state: could not write snapshot in %s: %w", t.dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, snapPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if dirFd, err := os.Open(t.dir); err == nil {
		_ = dirFd.Sync()
		_ = dirFd.Close()
	}
	if err := t.journal.Truncate(0); err != nil {
		return err
	}
	t.eventsSinceCompact = 0
	return nil
}

func (t *Tracker) loadSnapshot(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("crawl state file %s is corrupted: %v. "+
			"Delete the state directory %s and re-run to start a fresh crawl", path, err, t.dir)
	}
	if snap.Version != snapshotVersion {
		return fmt.Errorf("crawl state file %s has unsupported version %d (expected %d). "+
			"Delete the state directory %s and re-run to start a fresh crawl", path, snap.Version, snapshotVersion, t.dir)
	}
	for _, s := range snap.Seeds {
		if _, ok := t.seeds[s]; !ok {
			t.seeds[s] = struct{}{}
			t.seedLst = append(t.seedLst, s)
		}
	}
	for _, norm := range snap.Done {
		t.done[norm] = struct{}{}
	}
	t.reused = len(snap.Done)
	for norm, rec := range snap.Failed {
		t.failed[norm] = rec
	}
	// snapshot pending entries carry no order; the journal re-establishes
	// order for anything recorded after the last compaction, and within the
	// snapshot map order is deterministic enough (map iteration is random,
	// so sort by URL for stability)
	for _, norm := range sortedKeys(snap.Pending) {
		entry := snap.Pending[norm]
		t.pendingIdx[norm] = t.pendingLst.PushBack(entry)
	}
	return nil
}

func (t *Tracker) replayJournal(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	var offset, goodOffset int64
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" {
				var ev journalEvent
				if jerr := json.Unmarshal([]byte(trimmed), &ev); jerr != nil {
					if errors.Is(err, io.EOF) {
						// torn tail from a kill mid-write: truncate it
						return truncateFile(path, goodOffset)
					}
					return fmt.Errorf("crawl state journal %s is corrupted at offset %d: %v. "+
						"Delete the state directory %s and re-run to start a fresh crawl", path, offset, jerr, t.dir)
				}
				t.applyEvent(ev)
			}
			offset += int64(len(line))
			goodOffset = offset
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (t *Tracker) applyEvent(ev journalEvent) {
	switch ev.Op {
	case "seed":
		if ev.Entry == nil {
			return
		}
		norm, err := utils.NormalizeURL(ev.Entry.URL)
		if err != nil {
			return
		}
		if _, ok := t.seeds[norm]; !ok {
			t.seeds[norm] = struct{}{}
			t.seedLst = append(t.seedLst, norm)
		}
		t.applyPending(norm, *ev.Entry)
	case "pending":
		if ev.Entry == nil {
			return
		}
		norm, err := utils.NormalizeURL(ev.Entry.URL)
		if err != nil {
			return
		}
		t.applyPending(norm, *ev.Entry)
	case "done":
		delete(t.failed, ev.URL)
		t.removePendingLocked(ev.URL)
		if _, ok := t.done[ev.URL]; !ok {
			t.done[ev.URL] = struct{}{}
			t.reused++
		}
	case "failed":
		t.removePendingLocked(ev.URL)
		rec := t.failed[ev.URL]
		rec.Kind = ev.Kind
		rec.Message = ev.Message
		rec.Count++
		t.failed[ev.URL] = rec
	}
}

func (t *Tracker) applyPending(norm string, entry PendingEntry) {
	if _, ok := t.done[norm]; ok {
		return
	}
	if _, ok := t.pendingIdx[norm]; ok {
		return
	}
	t.pendingIdx[norm] = t.pendingLst.PushBack(entry)
}

// classifyError buckets a fetch failure into a stable, traceable kind.
func classifyError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	case strings.Contains(msg, "connection reset"):
		return "connection_reset"
	case strings.Contains(msg, "connection refused"):
		return "connection_refused"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "dns"):
		return "dns"
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "tls"):
		return "tls"
	default:
		return "other"
	}
}

func probeWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("crawl state directory %s is not writable: %v", dir, err)
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("crawl state directory %s is not writable: %v", dir, err)
	}
	name := probe.Name()
	if _, err := probe.WriteString("ok"); err != nil {
		_ = probe.Close()
		_ = os.Remove(name)
		return fmt.Errorf("crawl state directory %s is not writable: %v", dir, err)
	}
	if err := probe.Sync(); err != nil {
		_ = probe.Close()
		_ = os.Remove(name)
		return fmt.Errorf("crawl state directory %s is not writable: %v", dir, err)
	}
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

func truncateFile(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Truncate(size)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func orGet(method string) string {
	if method == "" {
		return http.MethodGet
	}
	return method
}

func sortedKeys(m map[string]PendingEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
