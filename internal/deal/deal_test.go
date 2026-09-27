package deal

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite deal-v1.schema.json from the Deal struct")

// The committed schema IS the generated one: change Deal or meta, run with
// -update, and commit both. A schema that drifted from the struct fails here.
func TestSchemaFileIsGenerated(t *testing.T) {
	got, err := GenerateSchema()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("deal-v1.schema.json", got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(got, SchemaFile) {
		t.Fatalf("deal-v1.schema.json is stale: run go test ./internal/deal -run TestSchemaFileIsGenerated -update\n--- generated ---\n%s", got)
	}
}

func TestSchemaMetaCoversEveryField(t *testing.T) {
	fields := map[string]bool{}
	for _, f := range structFields() {
		fields[f.name] = true
		if _, ok := meta[f.name]; !ok {
			t.Errorf("field %q has no schema metadata", f.name)
		}
	}
	for k := range meta {
		if !fields[k] {
			t.Errorf("schema metadata %q names no Deal field", k)
		}
	}
}

// The schema's property and required sets are exactly the struct's.
func TestSchemaMatchesStruct(t *testing.T) {
	s := SchemaObject()
	props := s["properties"].(map[string]any)
	var want, gotProps []string
	var wantReq []string
	for _, f := range structFields() {
		want = append(want, f.name)
		if f.required {
			wantReq = append(wantReq, f.name)
		}
	}
	for k := range props {
		gotProps = append(gotProps, k)
	}
	sort.Strings(want)
	sort.Strings(gotProps)
	if !reflect.DeepEqual(want, gotProps) {
		t.Fatalf("schema properties %v, struct fields %v", gotProps, want)
	}
	var gotReq []string
	for _, r := range s["required"].([]any) {
		gotReq = append(gotReq, r.(string))
	}
	if !reflect.DeepEqual(wantReq, gotReq) {
		t.Fatalf("schema required %v, struct %v", gotReq, wantReq)
	}
	if want := []string{"category", "title", "price", "url"}; !reflect.DeepEqual(wantReq, want) {
		t.Fatalf("required fields are %v, the approved spec says %v", wantReq, want)
	}
	if s["additionalProperties"] != false {
		t.Fatal("the schema must refuse unknown fields")
	}
}

func TestExamplesDecode(t *testing.T) {
	for _, ex := range []string{ExampleWine, ExampleHDD} {
		if _, r := Decode([]byte(ex)); r != ReasonNone {
			t.Fatalf("example refused %s: %s", r, ex)
		}
		if len(ex) > MaxLineBytes {
			t.Fatal("example over the line limit")
		}
	}
	if ex := Examples(); len(ex) != 2 || ex[0]["category"] != "wine" || ex[1]["category"] != "hdd" {
		t.Fatalf("want one wine and one hdd example, got %v", ex)
	}
}

// The human docs show the canonical examples verbatim, and every doc about
// the format is ASCII.
func TestDocsCarryTheExamples(t *testing.T) {
	doc, err := os.ReadFile("../../docs/deal-submission.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range []string{ExampleWine, ExampleHDD} {
		if !strings.Contains(string(doc), ex) {
			t.Errorf("docs/deal-submission.md does not show the example %s", ex)
		}
	}
	for _, p := range []string{"../../docs/deal-submission.md", "../../docs/deal-submission-skill.md",
		"../../docs/design/2026-09-26-deal-submission-jsonl.md", "deal-v1.schema.json"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range b {
			if c > 0x7e || (c < 0x20 && c != '\n' && c != '\t') {
				t.Fatalf("%s: non-ASCII byte 0x%02x at %d", p, c, i)
			}
		}
	}
}

func TestDecodeReasons(t *testing.T) {
	ok := `"category":"hdd","title":"Example 18TB drive","price":"199.99","url":"https://store.example.com/x"`
	wine := `"category":"wine","title":"Example Syrah","price":24,"url":"https://shop.example.com/y"`
	cases := []struct {
		line string
		want Reason
	}{
		{`{` + ok + `}`, ReasonNone},
		{`{` + wine + `}`, ReasonNone},
		{`{` + ok + `,"schema":"nagus.deal/v1"}`, ReasonNone},
		{`{` + ok + `,"currency":"EUR","gtin":"0123456789012","condition":"used","capacity_tb":18}`, ReasonNone},
		{`{` + wine + `,"vintage":2019,"bottle_ml":1500,"gtin":"12345678"}`, ReasonNone},
		{`{` + ok, ReasonBadJSON},
		{`{` + ok + `} trailing`, ReasonBadJSON},
		{`{` + ok + `}{}`, ReasonBadJSON},
		{`{` + ok + `,"colour":"red"}`, ReasonUnknownField},
		{`{"Category":"hdd","title":"x","price":"1","url":"https://a.example"}`, ReasonUnknownField},
		{`{` + wine + `,"vintage":"2019"}`, ReasonBadType},
		{`{` + ok + `,"capacity_tb":"18"}`, ReasonBadType},
		{`{"category":"hdd","price":"1","url":"https://a.example"}`, ReasonMissingField},
		{`{"category":"hdd","title":"  ","price":"1","url":"https://a.example"}`, ReasonMissingField},
		{`{` + ok + `,"schema":"nagus.deal/v2"}`, ReasonBadSchema},
		{`{"category":"ssd","title":"x","price":"1","url":"https://a.example"}`, ReasonBadCategory},
		{`{` + ok + `,"vintage":2019}`, ReasonFieldNotForCategory},
		{`{` + wine + `,"capacity_tb":4}`, ReasonFieldNotForCategory},
		{`{` + wine + `,"mpn":"X1"}`, ReasonFieldNotForCategory},
		{`{"category":"hdd","title":"x","price":"0","url":"https://a.example"}`, ReasonBadPrice},
		{`{"category":"hdd","title":"x","price":"-5","url":"https://a.example"}`, ReasonBadPrice},
		{`{"category":"hdd","title":"x","price":12.999,"url":"https://a.example"}`, ReasonBadPrice},
		{`{"category":"hdd","title":"x","price":1e3,"url":"https://a.example"}`, ReasonBadPrice},
		{`{"category":"hdd","title":"x","price":"$19","url":"https://a.example"}`, ReasonBadPrice},
		{`{"category":"hdd","title":"x","price":"1,299.00","url":"https://a.example"}`, ReasonBadPrice},
		{`{"category":"hdd","title":"x","price":"10000001","url":"https://a.example"}`, ReasonBadPrice},
		{`{"category":"hdd","title":"x","price":true,"url":"https://a.example"}`, ReasonBadPrice},
		{`{` + ok + `,"currency":"usd"}`, ReasonBadCurrency},
		{`{"category":"hdd","title":"x","price":"1","url":"http://a.example/x"}`, ReasonBadURL},
		{`{"category":"hdd","title":"x","price":"1","url":"https:///nohost"}`, ReasonBadURL},
		{`{"category":"hdd","title":"x","price":"1","url":"https://user:pw@a.example/"}`, ReasonBadURL},
		{`{"category":"hdd","title":"x","price":"1","url":"javascript:alert(1)"}`, ReasonBadURL},
		{`{` + ok + `,"gtin":"123"}`, ReasonBadValue},
		{`{` + ok + `,"condition":"like new"}`, ReasonBadValue},
		{`{` + ok + `,"capacity_tb":0}`, ReasonBadValue},
		{`{` + wine + `,"vintage":1492}`, ReasonBadValue},
		{`{` + wine + `,"bottle_ml":5}`, ReasonBadValue},
		{`{"category":"hdd","title":"x\u0007y","price":"1","url":"https://a.example"}`, ReasonBadValue},
		{`{"category":"hdd","title":"` + strings.Repeat("t", MaxTitleLen+1) + `","price":"1","url":"https://a.example"}`, ReasonBadValue},
		{`{` + ok + `,"note":"` + strings.Repeat("n", MaxLineBytes) + `"}`, ReasonLineTooLong},
	}
	for _, c := range cases {
		_, got := Decode([]byte(c.line))
		if got != c.want {
			l := c.line
			if len(l) > 120 {
				l = l[:120] + "..."
			}
			t.Errorf("Decode(%s) = %q, want %q", l, got, c.want)
		}
	}
}

func TestPriceForms(t *testing.T) {
	for in, want := range map[string]int64{`"24.99"`: 2499, `24.99`: 2499, `"24.9"`: 2490, `24`: 2400, `"0.5"`: 50, `"1000000"`: 100000000} {
		var d Deal
		if err := json.Unmarshal([]byte(`{"price":`+in+`}`), &d); err != nil {
			t.Fatal(in, err)
		}
		if got, ok := d.Price.Cents(); !ok || got != want {
			t.Errorf("price %s -> %d %v, want %d", in, got, ok, want)
		}
	}
}

func TestToRawMapsHintsPerCategory(t *testing.T) {
	d, r := Decode([]byte(ExampleHDD))
	if r != ReasonNone {
		t.Fatal(r)
	}
	raw := d.ToRaw("m1@example.org#L3", "caspar")
	if raw.PriceCents != 21999 || raw.Currency != "USD" || raw.ConditionRaw != "refurb" || raw.SourceKey != "m1@example.org#L3" {
		t.Fatalf("hdd raw %+v", raw)
	}
	for k, v := range map[string]string{"brand": "Example Digital", "mpn": "EX20T-0001", "capacity_tb": "20",
		"seller": "Example Store", AspectSubmittedBy: "caspar", AspectSchema: SchemaID} {
		if raw.Aspects[k] != v {
			t.Errorf("hdd aspect %s = %q, want %q", k, raw.Aspects[k], v)
		}
	}
	w, _ := Decode([]byte(ExampleWine))
	wr := w.ToRaw("m1@example.org#L2", "steve")
	if wr.Aspects["wine_producer"] != "Example Cellars" || wr.Aspects["brand"] != "" ||
		wr.Aspects["vintage"] != "2021" || wr.Aspects["bottle_ml"] != "750" {
		t.Fatalf("wine aspects %v: brand must be the producer (name hint), never the structured brand", wr.Aspects)
	}
	if wr.Body != "in store only, ends Sunday" || wr.Title == "" || wr.SourceURL != "https://shop.example.com/syrah-2021" {
		t.Fatalf("wine raw %+v", wr)
	}
}
