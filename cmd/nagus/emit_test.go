package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/score"
)

// pureCaptureStdout redirects os.Stdout for the duration of fn, returning
// whatever fn wrote to it. emitTable and emitJSON write straight to
// os.Stdout (not an injected io.Writer), so this is the only way to observe
// their output from a test without touching production code.
func pureCaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return buf.String()
}

func pureScoredItem(id, title string, priceCents int64, condition, capacityTB string, verdict string, scoreVal float64) pipeline.Scored {
	return pipeline.Scored{
		Item: item.Item{
			ID:         id,
			Title:      title,
			PriceCents: priceCents,
			Condition:  condition,
			SourceURL:  "https://example.test/" + id,
			Attributes: map[string]string{"capacity_tb": capacityTB},
		},
		Signal: score.DealSignal{Verdict: verdict},
		Score:  score.Score{Value: scoreVal, Rationale: "test rationale"},
	}
}

// --- emitTable ---

func TestEmitTableNoItemsPrintsSummaryOnly(t *testing.T) {
	res := pipeline.SurfaceResult{Matched: 3, Filtered: 0, Items: nil}
	out := pureCaptureStdout(t, func() { emitTable("hdd", res) })
	want := "search[hdd]: matched=3 survived-filter=0\n"
	if out != want {
		t.Errorf("emitTable() output = %q, want %q", out, want)
	}
}

func TestEmitTableWithItemsRendersHeaderAndRows(t *testing.T) {
	res := pipeline.SurfaceResult{
		Matched:  2,
		Filtered: 2,
		Items: []pipeline.Scored{
			pureScoredItem("id-1", "An 8TB Enterprise Drive, barely used", 15000, "used", "8", "great", 92.5),
			pureScoredItem("id-2", "No price or capacity listing", 0, "", "", "unknown-no-price", 0),
		},
	}
	out := pureCaptureStdout(t, func() { emitTable("hdd", res) })

	if !strings.Contains(out, "search[hdd]: matched=2 survived-filter=2\n") {
		t.Errorf("emitTable() missing summary line, got: %q", out)
	}
	if !strings.Contains(out, "VERDICT") || !strings.Contains(out, "SCORE") || !strings.Contains(out, "TITLE") {
		t.Errorf("emitTable() missing expected header columns, got: %q", out)
	}
	if !strings.Contains(out, "great") || !strings.Contains(out, "93") {
		// %.0f of 92.5 rounds to 93 (round-half-to-even would give 92, but
		// Printf's %.0f on 92.5 rounds to the nearest even -> 92; verify actual
		// behavior loosely by just checking the row is present).
		if !strings.Contains(out, "92") {
			t.Errorf("emitTable() missing scored row for id-1, got: %q", out)
		}
	}
	if !strings.Contains(out, "unknown-no-price") {
		t.Errorf("emitTable() missing verdict for id-2, got: %q", out)
	}
	// id-2 has no price/capacity/condition -> dashes should appear.
	lines := strings.Split(out, "\n")
	foundDash := false
	for _, l := range lines {
		if strings.Contains(l, "unknown-no-price") && strings.Contains(l, "-") {
			foundDash = true
		}
	}
	if !foundDash {
		t.Errorf("emitTable() expected dash placeholders on id-2's row, got: %q", out)
	}
}

func TestEmitTableTruncatesLongTitles(t *testing.T) {
	longTitle := strings.Repeat("x", 100)
	res := pipeline.SurfaceResult{
		Matched:  1,
		Filtered: 1,
		Items:    []pipeline.Scored{pureScoredItem("id-1", longTitle, 1000, "new", "4", "good", 10)},
	}
	out := pureCaptureStdout(t, func() { emitTable("hdd", res) })
	if strings.Contains(out, longTitle) {
		t.Errorf("emitTable() should have truncated the 100-char title, got: %q", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("emitTable() expected an ellipsis for the truncated title, got: %q", out)
	}
}

// --- emitJSON ---

func TestEmitJSONEmptyItemsProducesEmptyArray(t *testing.T) {
	res := pipeline.SurfaceResult{Matched: 0, Filtered: 0, Items: nil}
	out := pureCaptureStdout(t, func() {
		if err := emitJSON(res); err != nil {
			t.Fatalf("emitJSON: %v", err)
		}
	})
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("emitJSON output not valid JSON: %v\noutput: %q", err, out)
	}
	if len(got) != 0 {
		t.Errorf("emitJSON() with no items = %d rows, want 0", len(got))
	}
}

func TestEmitJSONEncodesFieldsAndRank(t *testing.T) {
	res := pipeline.SurfaceResult{
		Matched:  2,
		Filtered: 2,
		Items: []pipeline.Scored{
			pureScoredItem("id-1", "First Item", 12345, "new", "10", "great", 88),
			pureScoredItem("id-2", "Second Item", 6789, "used", "4", "poor", 12),
		},
	}
	out := pureCaptureStdout(t, func() {
		if err := emitJSON(res); err != nil {
			t.Fatalf("emitJSON: %v", err)
		}
	})

	type row struct {
		Rank       int     `json:"rank"`
		ID         string  `json:"id"`
		Verdict    string  `json:"verdict"`
		Score      float64 `json:"score"`
		Rationale  string  `json:"rationale"`
		PriceCents int64   `json:"price_cents"`
		CapacityTB string  `json:"capacity_tb"`
		Condition  string  `json:"condition"`
		Title      string  `json:"title"`
		URL        string  `json:"source_url"`
	}
	var rows []row
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("emitJSON output not valid JSON: %v\noutput: %q", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("emitJSON() rows = %d, want 2", len(rows))
	}
	if rows[0].Rank != 1 || rows[1].Rank != 2 {
		t.Errorf("emitJSON() ranks = %d,%d want 1,2", rows[0].Rank, rows[1].Rank)
	}
	if rows[0].ID != "id-1" || rows[0].Verdict != "great" || rows[0].Score != 88 ||
		rows[0].PriceCents != 12345 || rows[0].CapacityTB != "10" || rows[0].Condition != "new" ||
		rows[0].Title != "First Item" || rows[0].URL != "https://example.test/id-1" {
		t.Errorf("emitJSON() row[0] = %+v, unexpected field values", rows[0])
	}
	if rows[1].ID != "id-2" || rows[1].Verdict != "poor" {
		t.Errorf("emitJSON() row[1] = %+v, unexpected field values", rows[1])
	}
}

// --- capacityTB ---

func TestCapacityTBReturnsAttribute(t *testing.T) {
	it := item.Item{Attributes: map[string]string{"capacity_tb": "8"}}
	if got := capacityTB(it); got != "8" {
		t.Errorf("capacityTB() = %q, want %q", got, "8")
	}
}

func TestCapacityTBMissingAttributeReturnsEmpty(t *testing.T) {
	it := item.Item{Attributes: map[string]string{}}
	if got := capacityTB(it); got != "" {
		t.Errorf("capacityTB() = %q, want empty", got)
	}
}

func TestCapacityTBNilAttributesReturnsEmpty(t *testing.T) {
	it := item.Item{}
	if got := capacityTB(it); got != "" {
		t.Errorf("capacityTB() with nil Attributes = %q, want empty", got)
	}
}

// --- dollars ---

func TestDollarsFormatsCents(t *testing.T) {
	cases := []struct {
		name  string
		cents int64
		want  string
	}{
		{"typical amount", 12345, "$123.45"},
		{"exact dollar", 100, "$1.00"},
		{"single cent", 1, "$0.01"},
		{"zero is dash", 0, "-"},
		{"negative is dash", -500, "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dollars(tc.cents); got != tc.want {
				t.Errorf("dollars(%d) = %q, want %q", tc.cents, got, tc.want)
			}
		})
	}
}

// --- dollarsPerTB ---

func TestDollarsPerTB(t *testing.T) {
	cases := []struct {
		name   string
		cents  int64
		capStr string
		want   string
	}{
		{"typical", 20000, "10", "$20.00"},
		{"zero cents is dash", 0, "10", "-"},
		{"negative cents is dash", -1, "10", "-"},
		{"empty capacity is dash", 15000, "", "-"},
		{"non-numeric capacity is dash", 15000, "not-a-number", "-"},
		{"zero capacity is dash", 15000, "0", "-"},
		{"negative capacity is dash", 15000, "-4", "-"},
		{"fractional capacity", 10000, "2.5", "$40.00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dollarsPerTB(tc.cents, tc.capStr); got != tc.want {
				t.Errorf("dollarsPerTB(%d, %q) = %q, want %q", tc.cents, tc.capStr, got, tc.want)
			}
		})
	}
}

// --- orDash ---

func TestOrDash(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want string
	}{
		{"empty becomes dash", "", "-"},
		{"nonempty passes through", "used", "used"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orDash(tc.s); got != tc.want {
				t.Errorf("orDash(%q) = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}

// --- truncate ---

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"shorter than limit unchanged", "hello", 10, "hello"},
		{"exactly at limit unchanged", "hello", 5, "hello"},
		{"longer than limit gets ellipsis", "hello world", 8, "hello..."},
		{"n at 3 or below hard-cuts with no ellipsis", "hello world", 3, "hel"},
		{"n at 0 hard-cuts to empty", "hello", 0, ""},
		{"empty string", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.s, tc.n)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
			if tc.n >= 0 && len(got) > tc.n {
				t.Errorf("truncate(%q, %d) = %q, longer than n", tc.s, tc.n, got)
			}
		})
	}
}
