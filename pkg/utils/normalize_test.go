package utils

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		expected string
	}{
		{"fragment stripped", "http://example.com/page#section", "http://example.com/page"},
		{"host case unified", "http://EXAMPLE.com/page", "http://example.com/page"},
		{"scheme case unified", "HTTP://example.com/page", "http://example.com/page"},
		{"double slashes collapsed", "http://example.com//a///b", "http://example.com/a/b"},
		{"empty path becomes root", "http://example.com", "http://example.com/"},
		{"query preserved", "http://example.com/p?a=1&b=2", "http://example.com/p?a=1&b=2"},
		{"path case preserved", "http://example.com/CaseSensitive", "http://example.com/CaseSensitive"},
		{"port preserved", "http://example.com:8080//x#f", "http://example.com:8080/x"},
		{"https kept distinct", "https://example.com/a", "https://example.com/a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeURL(tt.in)
			require.NoError(t, err)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestNormalizeURLSamePageSameIdentity(t *testing.T) {
	variants := []string{
		"http://example.com/page",
		"http://example.com/page#intro",
		"HTTP://EXAMPLE.COM//page#other",
		"http://example.com//page",
	}
	var first string
	for i, v := range variants {
		norm, err := NormalizeURL(v)
		require.NoError(t, err)
		if i == 0 {
			first = norm
			continue
		}
		require.Equal(t, first, norm, "variant %q should normalize to the same identity", v)
	}
}
