package deal

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// SchemaPath is where the schema is served, read-only, by nagus serve.
const SchemaPath = "/schemas/deal/v1.json"

// SchemaFile is the committed canonical schema (deal-v1.schema.json), served
// byte for byte at SchemaPath. TestSchemaFileIsGenerated fails when it
// differs from GenerateSchema, so the file and the struct cannot drift;
// `go test ./internal/deal -run TestSchemaFileIsGenerated -update` rewrites
// it after a deliberate change to Deal.
//
//go:embed deal-v1.schema.json
var SchemaFile []byte

// fieldMeta is the part of a field's schema that the Go type cannot say:
// the description and the value rules. Every limit here is one of the
// constants the validator enforces.
type fieldMeta struct {
	Description  string
	Enum         []string
	Const        string
	Pattern      string
	MinLength    int
	MaxLength    int
	Minimum      *float64
	Maximum      *float64
	ExclusiveMin *float64
}

func num(f float64) *float64 { return &f }

// meta is keyed by JSON field name. TestSchemaMetaCoversEveryField fails when a
// Deal field has no entry or an entry names no field.
var meta = map[string]fieldMeta{
	"schema": {Description: "Optional format marker. If present it must be exactly " + SchemaID + ".",
		Const: SchemaID},
	"category": {Description: "Which nagus category the deal is for. Each needs a deal source enabled for it.",
		Enum: Categories},
	"title": {Description: "What is for sale, as the store names it: producer, wine, vintage, size; or brand, model, capacity. Free text; untrusted.",
		MinLength: 1, MaxLength: MaxTitleLen},
	"price": {Description: "The price in major units (dollars, not cents), as a decimal string \"24.99\" or a JSON number 24.99. At most 2 decimals; no sign, exponent or separators.",
		Pattern: PricePattern},
	"url": {Description: "An https link to the deal, or to the store/product page it is from. Lower-case https:// and host, printable ASCII (an IDN host as punycode), a public DNS host name: no IP address in any form, no userinfo, no port but :443, not .local/.internal/.lan/.home/.corp/.svc/.test/.invalid/.arpa/.onion. Every percent-escape must be valid and at most double-encoded.",
		MinLength: 1, MaxLength: MaxURLLen, Pattern: URLPattern},
	"currency": {Description: "ISO 4217 currency code. Default " + DefaultCurrency + ".",
		Pattern: CurrencyPattern},
	"seller": {Description: "The store or merchant selling it (e.g. a shop name). Free text; untrusted.",
		MaxLength: MaxNameLen},
	"brand": {Description: "hdd: the drive manufacturer (a product hint for identity). wine: the producer/winery, sent with the title as the wine's name hint.",
		MaxLength: MaxNameLen},
	"mpn": {Description: "hdd only: the manufacturer part number (a product hint for identity).",
		MaxLength: MaxMPNLen},
	"gtin": {Description: "GTIN-8, UPC-A, EAN-13 or GTIN-14 digits. hdd: a product hint; wine: stored, not used for identity.",
		Pattern: GTINPattern},
	"vintage": {Description: "wine only: the vintage year.",
		Minimum: num(MinVintage), Maximum: num(MaxVintage)},
	"bottle_ml": {Description: "wine only: bottle size in millilitres (750 standard, 1500 magnum).",
		Minimum: num(MinBottleML), Maximum: num(MaxBottleML)},
	"capacity_tb": {Description: "hdd only: capacity in terabytes.",
		Minimum: num(MinCapacityTB), Maximum: num(MaxCapacityTB)},
	"condition": {Description: "hdd only: the drive's condition.",
		Enum: Conditions},
	"note": {Description: "Free-text context (\"in store only\", \"ends Sunday\"). Stored with the deal; never used for identity. Untrusted.",
		MaxLength: MaxNoteLen},
}

type structField struct {
	name     string
	typ      reflect.Type
	required bool
}

// structFields lists Deal's JSON fields in declaration order.
func structFields() []structField {
	t := reflect.TypeFor[Deal]()
	out := make([]structField, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out = append(out, structField{name: name, typ: f.Type, required: f.Tag.Get("deal") == "required"})
	}
	return out
}

// jsonType maps a Deal field's Go type onto its JSON Schema type.
func jsonType(t reflect.Type) (any, error) {
	if t == reflect.TypeFor[Price]() {
		return []string{"string", "number"}, nil
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string", nil
	case reflect.Int, reflect.Int64:
		return "integer", nil
	case reflect.Float64:
		return "number", nil
	}
	return nil, fmt.Errorf("deal: no JSON Schema type for %s", t)
}

// orderedObject marshals its keys in the given order, so the generated file
// reads in schema order (required fields first, as declared) and is stable.
type orderedObject struct {
	keys []string
	vals map[string]any
}

func (o *orderedObject) set(k string, v any) {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *orderedObject) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

// GenerateSchema builds the JSON Schema (draft 2020-12) from Deal and meta.
func GenerateSchema() ([]byte, error) {
	props := &orderedObject{}
	var required []string
	for _, f := range structFields() {
		m, ok := meta[f.name]
		if !ok {
			return nil, fmt.Errorf("deal: field %q has no schema metadata", f.name)
		}
		typ, err := jsonType(f.typ)
		if err != nil {
			return nil, err
		}
		p := &orderedObject{}
		p.set("type", typ)
		p.set("description", m.Description)
		if m.Const != "" {
			p.set("const", m.Const)
		}
		if len(m.Enum) > 0 {
			p.set("enum", m.Enum)
		}
		if m.Pattern != "" {
			p.set("pattern", m.Pattern)
		}
		if m.MinLength > 0 {
			p.set("minLength", m.MinLength)
		}
		if m.MaxLength > 0 {
			p.set("maxLength", m.MaxLength)
		}
		if m.ExclusiveMin != nil {
			p.set("exclusiveMinimum", *m.ExclusiveMin)
		}
		if m.Minimum != nil {
			p.set("minimum", *m.Minimum)
		}
		if m.Maximum != nil {
			p.set("maximum", *m.Maximum)
		}
		props.set(f.name, p)
		if f.required {
			required = append(required, f.name)
		}
	}
	root := &orderedObject{}
	root.set("$schema", "https://json-schema.org/draft/2020-12/schema")
	root.set("$id", "urn:nagus:schema:deal:v1")
	root.set("title", SchemaID)
	root.set("description", fmt.Sprintf(
		"One deal submitted to the nagus deals mailbox. Send one object per line in the text/plain body; "+
			"at most %d deal lines per message and %d bytes per line. Unknown fields refuse the line. "+
			"wine-only: vintage, bottle_ml. hdd-only: mpn, capacity_tb, condition.",
		MaxLinesPerMessage, MaxLineBytes))
	root.set("type", "object")
	root.set("required", required)
	root.set("additionalProperties", false)
	root.set("properties", props)
	// Category-only fields: forbidden (false) on every other category.
	var rules []any
	for _, cat := range Categories {
		forbid := &orderedObject{}
		for _, other := range Categories {
			if other == cat {
				continue
			}
			for _, f := range CategoryOnly[other] {
				forbid.set(f, false)
			}
		}
		rules = append(rules, map[string]any{
			"if":   map[string]any{"properties": map[string]any{"category": map[string]any{"const": cat}}},
			"then": map[string]any{"properties": forbid},
		})
	}
	root.set("allOf", rules)
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// SchemaObject is the committed schema decoded, for embedding in an MCP
// result.
func SchemaObject() map[string]any {
	var m map[string]any
	if err := json.Unmarshal(SchemaFile, &m); err != nil {
		panic(fmt.Sprintf("deal: the embedded schema is not JSON: %v", err))
	}
	return m
}
