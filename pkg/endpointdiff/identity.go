// Package endpointdiff computes deterministic week-over-week changes between
// two katana endpoint inventories (jsonl files produced with -kb-endpoints).
//
// The package is offline and pure: it never performs network IO and spawns no
// goroutines. All ordering is derived from explicit sorts (never from map
// iteration or input arrival order), so comparing the same two inputs twice
// produces a byte-identical report.
package endpointdiff

import (
	"net/url"
	"sort"
	"strings"

	"github.com/projectdiscovery/katana/pkg/utils"
)

// SchemaVersion is bumped whenever the report or manifest on-disk schema
// changes in an incompatible way.
const SchemaVersion = 1

// Identity builds the stable identity key of an endpoint.
//
// The key is "METHOD scheme://host/templated-path" where every dynamic path
// segment (numeric ids, UUIDs, hashes, dates, ...) is replaced by a
// placeholder using the same heuristic table as the crawler's -fsu filter
// (utils.NormalizePathSegment). Consequently /users/1 and /users/2 share one
// identity and never appear as a daily add/remove pair.
//
// Query strings are deliberately excluded from the identity: parameter-set
// changes on the same path are reported separately as changes instead of
// splitting one endpoint into two. Hostnames are lowercased; userinfo and
// fragments (already stripped by the extractor) are ignored.
//
// Identity is a pure function of method and rawURL.
func Identity(method, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return method + " " + u.Scheme + "://" + strings.ToLower(u.Host) + TemplatePath(rawURL), nil
}

// TemplatePath returns the path portion of rawURL with dynamic segments
// replaced by placeholders. The returned string always starts with "/" (an
// empty path is reported as "/"). It is used both inside identities and for
// structural route-reorganization matching.
func TemplatePath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	p := u.Path
	if p == "" {
		return "/"
	}
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return "/"
	}
	segments := strings.Split(trimmed, "/")
	for i, seg := range segments {
		if placeholder, matched := utils.NormalizePathSegment(seg); matched {
			segments[i] = placeholder
		}
	}
	templated := "/" + strings.Join(segments, "/")
	if strings.HasSuffix(p, "/") {
		templated += "/"
	}
	return templated
}

// ParamNames returns the sorted, deduplicated set of query parameter names
// carried by rawURL. Sorting makes parameter-set comparison independent of
// the order parameters appeared in the crawled URL.
func ParamNames(rawURL string) ([]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	for name := range u.Query() {
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// sortedUnique returns vals sorted and deduplicated.
func sortedUnique(vals []string) []string {
	seen := make(map[string]struct{}, len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// setDiff returns the elements present in a but not in b, sorted.
func setDiff(a, b []string) []string {
	bset := make(map[string]struct{}, len(b))
	for _, v := range b {
		bset[v] = struct{}{}
	}
	var out []string
	for _, v := range a {
		if _, ok := bset[v]; !ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
