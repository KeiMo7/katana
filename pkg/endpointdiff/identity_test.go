package endpointdiff

import (
	"testing"
)

func TestIdentity_DynamicSegmentsDoNotChangeKey(t *testing.T) {
	// Acceptance: dynamic path segments (numeric ids, UUIDs) must not enter
	// the identity key, otherwise the same endpoint looks new every run.
	cases := []struct {
		name string
		a    string
		b    string
	}{
		{"numeric id", "https://api.example.com/users/1", "https://api.example.com/users/2"},
		{"uuid", "https://api.example.com/orders/550e8400-e29b-41d4-a716-446655440000",
			"https://api.example.com/orders/660e8400-e29b-41d4-a716-446655440111"},
		{"nested numeric", "https://api.example.com/v1/users/123/posts/456",
			"https://api.example.com/v1/users/789/posts/999"},
		{"hash segment", "https://cdn.example.com/files/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"https://cdn.example.com/files/aaaaaaaa98fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idA, err := Identity("GET", tc.a)
			if err != nil {
				t.Fatalf("Identity(a): %v", err)
			}
			idB, err := Identity("GET", tc.b)
			if err != nil {
				t.Fatalf("Identity(b): %v", err)
			}
			if idA != idB {
				t.Errorf("identities differ for the same logical endpoint:\n  %s\n  %s", idA, idB)
			}
		})
	}
}

func TestIdentity_DifferentPathsAndMethodsDiffer(t *testing.T) {
	base := "https://api.example.com/users"
	id, _ := Identity("GET", base)
	if other, _ := Identity("POST", base); other == id {
		t.Error("GET and POST on the same URL must have different identities")
	}
	if other, _ := Identity("GET", "https://api.example.com/accounts"); other == id {
		t.Error("different static paths must have different identities")
	}
	if other, _ := Identity("GET", "https://other.example.com/users"); other == id {
		t.Error("different hosts must have different identities")
	}
}

func TestIdentity_QueryParamsExcluded(t *testing.T) {
	// Parameter-set changes are reported as changes, not identity splits.
	idA, _ := Identity("GET", "https://api.example.com/items?a=1&b=2")
	idB, _ := Identity("GET", "https://api.example.com/items?a=9&c=3")
	if idA != idB {
		t.Errorf("query strings must not enter identity: %q vs %q", idA, idB)
	}
}

func TestTemplatePath(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com/users/42":  "/users/{num}",
		"https://api.example.com/users/42/": "/users/{num}/",
		"https://api.example.com/":          "/",
		"https://api.example.com":           "/",
		"https://api.example.com/orders/550e8400-e29b-41d4-a716-446655440000/items": "/orders/{uuid}/items",
		"https://api.example.com/static/app.js":                                     "/static/app.js",
	}
	for raw, want := range cases {
		if got := TemplatePath(raw); got != want {
			t.Errorf("TemplatePath(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestParamNames_SortedAndDeduped(t *testing.T) {
	got, err := ParamNames("https://api.example.com/x?b=2&a=1&b=3&a=4")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
