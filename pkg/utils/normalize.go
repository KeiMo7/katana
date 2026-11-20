package utils

import (
	"net/url"
	"strings"
)

// NormalizeURL returns a canonical identity for rawURL used to decide whether
// two spellings refer to the same page. It:
//   - lowercases the scheme and host (both are case-insensitive per RFC 3986)
//   - strips the fragment identifier (#section), which is client-side only
//   - collapses repeated slashes in the path ("/a//b" -> "/a/b")
//   - treats an empty path as "/"
//
// The path and query are otherwise left untouched: path case is significant
// on some servers, and query parameters may change the returned page.
func NormalizeURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	parsed.RawFragment = ""

	// parsed.Path is already percent-decoded; collapsing slashes on it is
	// safe because an encoded slash (%2F) stays encoded in RawPath only.
	if parsed.Path == "" {
		parsed.Path = "/"
	} else {
		parsed.Path = collapseSlashes(parsed.Path)
	}
	parsed.RawPath = ""
	return parsed.String(), nil
}

// collapseSlashes folds runs of "/" into a single "/".
func collapseSlashes(path string) string {
	if !strings.Contains(path, "//") {
		return path
	}
	var b strings.Builder
	b.Grow(len(path))
	prev := byte(0)
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '/' && prev == '/' {
			continue
		}
		b.WriteByte(c)
		prev = c
	}
	return b.String()
}
