package endpointdiff

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// maxLineSize bounds a single jsonl line. katana stores full raw requests and
// response bodies, so lines can be large; 64 MiB matches the crawler's default
// maximum response size.
const maxLineSize = 64 * 1024 * 1024

// Inventory is an aggregated week of endpoints keyed by identity.
type Inventory struct {
	// Entries is keyed by Identity and owns the aggregated Endpoint records.
	Entries map[string]*Endpoint
	// LinesRead counts physical jsonl lines consumed.
	LinesRead int
	// LinesWithoutEndpoints counts well-formed lines that carried no
	// knowledgebase.endpoints payload (HTML pages, error-only lines, ...).
	LinesWithoutEndpoints int
}

// jsonLine is the minimal projection of an output.Result jsonl line the
// reader needs. Unknown fields are ignored, so unrelated output changes never
// break the diff.
type jsonLine struct {
	Request *struct {
		Method string `json:"method"`
		URL    string `json:"endpoint"`
		Source string `json:"source"`
	} `json:"request"`
	Response *struct {
		KnowledgeBase map[string]json.RawMessage `json:"knowledgebase"`
	} `json:"response"`
}

// kbEndpoint is the projection of one knowledgebase.endpoints element.
type kbEndpoint struct {
	Class  string   `json:"class"`
	Method string   `json:"method"`
	URL    string   `json:"url"`
	Params []string `json:"params"`
}

// ReadInventory consumes a katana jsonl stream (produced with -j
// -kb-endpoints) and aggregates it into an Inventory. It reads strictly
// line-by-line and never holds the whole file in memory: only the aggregated
// endpoint records (one per identity) are retained, so memory usage is
// proportional to the number of distinct identities, not the file size.
//
// A malformed JSON line is a hard error naming the 1-based line number, so a
// truncated or corrupt inventory fails loudly instead of producing a partial
// diff. Well-formed lines without endpoints data are skipped and counted.
func ReadInventory(r io.Reader) (*Inventory, error) {
	inv := &Inventory{Entries: make(map[string]*Endpoint)}
	reader := bufio.NewReaderSize(r, 1024*1024)

	lineNo := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			inv.LinesRead++
			if len(line) > maxLineSize {
				return nil, fmt.Errorf("line %d exceeds maximum size of %d bytes", lineNo, maxLineSize)
			}
			trimmed := strings.TrimSpace(string(line))
			if trimmed != "" {
				if err := inv.ingestLine(trimmed, lineNo); err != nil {
					return nil, err
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("reading inventory: %w", readErr)
		}
	}
	return inv, nil
}

func (inv *Inventory) ingestLine(line string, lineNo int) error {
	var row jsonLine
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		return fmt.Errorf("invalid JSON on line %d: %w", lineNo, err)
	}
	if row.Response == nil || len(row.Response.KnowledgeBase) == 0 {
		inv.LinesWithoutEndpoints++
		return nil
	}
	raw, ok := row.Response.KnowledgeBase["endpoints"]
	if !ok || len(raw) == 0 {
		inv.LinesWithoutEndpoints++
		return nil
	}
	entries, err := parseKbEndpoints(raw)
	if err != nil {
		return fmt.Errorf("invalid knowledgebase.endpoints on line %d: %w", lineNo, err)
	}
	if len(entries) == 0 {
		inv.LinesWithoutEndpoints++
		return nil
	}

	// The source page is a property of the whole crawled request, shared by
	// every endpoint the extractor emitted on it.
	requestSource := ""
	requestMethod := ""
	if row.Request != nil {
		requestSource = row.Request.Source
		requestMethod = row.Request.Method
	}
	for _, kb := range entries {
		if kb.URL == "" {
			continue
		}
		method := kb.Method
		if method == "" {
			method = requestMethod
		}
		if method == "" {
			method = "GET"
		}
		inv.merge(kb, method, requestSource)
	}
	return nil
}

// parseKbEndpoints accepts both the current single-object payload
// ({"class":...}) and an array of objects, so reports stay readable if a
// future extractor version emits several entries per response.
func parseKbEndpoints(raw json.RawMessage) ([]kbEndpoint, error) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed == "null" {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var entries []kbEndpoint
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
		return entries, nil
	}
	var single kbEndpoint
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, err
	}
	return []kbEndpoint{single}, nil
}

func (inv *Inventory) merge(kb kbEndpoint, method, requestSource string) {
	id, err := Identity(method, kb.URL)
	if err != nil {
		// An unparseable URL cannot take part in a stable comparison; skip
		// just this observation rather than corrupting the whole inventory.
		return
	}
	ep, ok := inv.Entries[id]
	if !ok {
		ep = &Endpoint{
			Identity:      id,
			Method:        method,
			URL:           kb.URL,
			TemplatedPath: TemplatePath(kb.URL),
			params:        make(map[string]struct{}),
			sources:       make(map[string]struct{}),
			classes:       make(map[string]struct{}),
		}
		inv.Entries[id] = ep
	}
	// Representative URL and class are the lexicographically smallest
	// observations, making aggregation independent of line arrival order.
	if kb.URL < ep.URL {
		ep.URL = kb.URL
		ep.TemplatedPath = TemplatePath(kb.URL)
	}
	if kb.Class != "" {
		ep.classes[kb.Class] = struct{}{}
	}
	for _, p := range kb.Params {
		if p != "" {
			ep.params[p] = struct{}{}
		}
	}
	if extra, err := ParamNames(kb.URL); err == nil {
		for _, p := range extra {
			ep.params[p] = struct{}{}
		}
	}
	if requestSource != "" {
		ep.sources[requestSource] = struct{}{}
	}
}

// finalizeAll materializes the deterministic sorted slices of every entry.
// It is idempotent and safe to call before comparisons or serialization.
func (inv *Inventory) finalizeAll() {
	for _, ep := range inv.Entries {
		ep.finalize()
	}
}

// finalize materializes the deterministic sorted slices and representative
// class from the internal set maps.
func (ep *Endpoint) finalize() {
	ep.Params = sortedKeys(ep.params)
	ep.Sources = sortedKeys(ep.sources)
	classes := sortedKeys(ep.classes)
	if len(classes) > 0 {
		ep.Class = classes[0]
	}
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
