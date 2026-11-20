package endpointdiff

import (
	"encoding/json"
	"strings"
	"testing"
)

// jsonlLine builds one katana-style output.Result jsonl line carrying a
// knowledgebase.endpoints payload.
func jsonlLine(t testing.TB, method, url, source string, params []string) string {
	t.Helper()
	line := map[string]any{
		"request": map[string]any{
			"method":   method,
			"endpoint": url,
			"source":   source,
		},
		"response": map[string]any{
			"status_code": 200,
			"knowledgebase": map[string]any{
				"endpoints": map[string]any{
					"class":  "rest",
					"method": method,
					"url":    url,
					"params": params,
				},
			},
		},
	}
	data, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(data)
}

// plainHTMLLine is a well-formed output line without endpoints data.
func plainHTMLLine(t *testing.T, url string) string {
	t.Helper()
	line := map[string]any{
		"request":  map[string]any{"endpoint": url},
		"response": map[string]any{"status_code": 200},
	}
	data, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(data)
}

func readInventory(t *testing.T, lines ...string) *Inventory {
	t.Helper()
	inv, err := ReadInventory(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("ReadInventory: %v", err)
	}
	return inv
}

func TestReadInventory_AggregatesByIdentity(t *testing.T) {
	inv := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/users/1", "https://api.example.com/home", nil),
		jsonlLine(t, "GET", "https://api.example.com/users/2", "https://api.example.com/home", nil),
		jsonlLine(t, "GET", "https://api.example.com/users/3", "https://api.example.com/other", nil),
	)
	if len(inv.Entries) != 1 {
		t.Fatalf("expected 1 aggregated identity for numeric-id variants, got %d", len(inv.Entries))
	}
	var ep *Endpoint
	for _, ep = range inv.Entries {
	}
	ep.finalize()
	if ep.URL != "https://api.example.com/users/1" {
		t.Errorf("representative URL = %q, want lexicographically smallest", ep.URL)
	}
	if len(ep.Sources) != 2 || ep.Sources[0] != "https://api.example.com/home" || ep.Sources[1] != "https://api.example.com/other" {
		t.Errorf("sources = %v, want sorted union of both source pages", ep.Sources)
	}
}

func TestReadInventory_MergesParamAndSourceUnions(t *testing.T) {
	inv := readInventory(t,
		jsonlLine(t, "GET", "https://api.example.com/items?a=1", "https://api.example.com/p1", []string{"a"}),
		jsonlLine(t, "GET", "https://api.example.com/items?b=2", "https://api.example.com/p2", []string{"b"}),
	)
	if len(inv.Entries) != 1 {
		t.Fatalf("expected 1 identity, got %d", len(inv.Entries))
	}
	var ep *Endpoint
	for _, ep = range inv.Entries {
	}
	ep.finalize()
	if got := strings.Join(ep.Params, ","); got != "a,b" {
		t.Errorf("params = %v, want sorted [a b]", ep.Params)
	}
	if len(ep.Sources) != 2 {
		t.Errorf("sources = %v, want both source pages", ep.Sources)
	}
}

func TestReadInventory_SkipsLinesWithoutEndpoints(t *testing.T) {
	inv := readInventory(t,
		plainHTMLLine(t, "https://api.example.com/about"),
		jsonlLine(t, "GET", "https://api.example.com/api/x", "", nil),
	)
	if len(inv.Entries) != 1 {
		t.Fatalf("expected 1 endpoint entry, got %d", len(inv.Entries))
	}
	if inv.LinesWithoutEndpoints != 1 {
		t.Errorf("LinesWithoutEndpoints = %d, want 1", inv.LinesWithoutEndpoints)
	}
}

func TestReadInventory_MalformedLineFailsWithLineNumber(t *testing.T) {
	input := jsonlLine(t, "GET", "https://api.example.com/ok", "", nil) + "\n{not json\n"
	if _, err := ReadInventory(strings.NewReader(input)); err == nil {
		t.Fatal("expected error on malformed JSON line, got nil")
	} else if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error should name line 2, got: %v", err)
	}
}

func TestReadInventory_OrderIndependence(t *testing.T) {
	lines := []string{
		jsonlLine(t, "GET", "https://api.example.com/a?z=1", "https://api.example.com/s1", []string{"z"}),
		jsonlLine(t, "GET", "https://api.example.com/b", "https://api.example.com/s2", nil),
		jsonlLine(t, "GET", "https://api.example.com/a?y=2", "https://api.example.com/s2", []string{"y"}),
	}
	inv1 := readInventory(t, lines...)
	inv2 := readInventory(t, lines[2], lines[0], lines[1])
	if len(inv1.Entries) != len(inv2.Entries) {
		t.Fatalf("inventory sizes differ with input order: %d vs %d", len(inv1.Entries), len(inv2.Entries))
	}
	for id, ep1 := range inv1.Entries {
		ep2, ok := inv2.Entries[id]
		if !ok {
			t.Fatalf("identity %q missing from reordered inventory", id)
		}
		ep1.finalize()
		ep2.finalize()
		if ep1.URL != ep2.URL || strings.Join(ep1.Params, ",") != strings.Join(ep2.Params, ",") ||
			strings.Join(ep1.Sources, ",") != strings.Join(ep2.Sources, ",") {
			t.Errorf("aggregation differs by input order for %q:\n  %+v\n  %+v", id, ep1, ep2)
		}
	}
}
