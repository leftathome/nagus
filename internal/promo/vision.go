package promo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The two prompts. They are worded independently on purpose: the agreement
// guard is only worth something if the second read is not the first one
// replayed. Both ask for the SAME JSON shape.
const (
	PromptA = "This image is one tile from a retailer's promotional email. If it shows a product for sale with a printed price, " +
		"describe it. Answer ONLY with JSON, no prose, no code fence: " +
		`{"products":[{"title":string,"price":string,"was_price":string|null,"brand":string|null,"currency":string|null}]}. ` +
		"title: the product name exactly as printed. price: the price a buyer pays now, digits and a decimal point only (e.g. \"589.99\"); when two prices are shown it is the sale price, never the struck-through or \"was\" one. " +
		"was_price: a struck-through, \"was\" or list price if one is printed, else null. brand: the maker, brand or winery if printed, else null. " +
		"currency: the ISO code (a $ sign is USD), else null. One entry per product. If no product with a printed price is shown, answer " +
		`{"products":[]}. Copy text exactly; never guess a value that is not printed.`
	PromptB = "You are checking a picture from a store's marketing email. Read the text in the picture and report each item offered " +
		"for sale whose selling price is visible, as JSON only, in exactly this form: " +
		`{"products":[{"title":"...","price":"...","was_price":null,"brand":null,"currency":null}]}. ` +
		"Use the item's printed name for title; the amount a buyer pays now, as a plain number such as 24.99, for price; any " +
		"crossed-out or regular price for was_price, otherwise null; the manufacturer or producer for brand, otherwise null; USD when " +
		`prices are in dollars, for currency. Return {"products":[]} if no item has a visible price. Transcribe what is printed; do not estimate.`
)

// MaxReadProducts bounds one answer.
const MaxReadProducts = 8

// ReadProduct is one product as a read reports it. Every field is untrusted.
type ReadProduct struct {
	Title    string  `json:"title"`
	Price    any     `json:"price"`
	WasPrice any     `json:"was_price"`
	Brand    *string `json:"brand"`
	Currency *string `json:"currency"`
}

// Reading is one parsed vision answer.
type Reading struct {
	Products []ReadProduct `json:"products"`
}

// Vision is an OpenAI-compatible chat-completions client for one model.
type Vision struct {
	BaseURL string // e.g. http://litellm.litellm.svc:4000/v1
	APIKey  string
	Model   string
	Client  *http.Client
	Timeout time.Duration
}

// Enabled reports whether every setting is present.
func (v *Vision) Enabled() bool {
	return v != nil && v.BaseURL != "" && v.APIKey != "" && v.Model != ""
}

// ErrVisionRefused is a 4xx answer that will not change on a retry: the
// request itself (this image) was refused. 408 and 429 are transient.
var ErrVisionRefused = errors.New("promo: vision endpoint refused the request")

// ErrVisionAuth is a 401 or 403: a key or permission problem of the
// deployment, not of any image. The parser stops the poll, refunds the calls
// and counts no attempt, so a bad key never marks the window as read.
var ErrVisionAuth = errors.New("promo: vision endpoint refused the key")

func refusedForGood(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
		return false
	}
	return code >= 400 && code < 500
}

// ErrBadAnswer is a vision answer that is not the strict JSON shape.
var ErrBadAnswer = errors.New("promo: vision answer is not the expected JSON")

// scrub removes the API key from an error text, whatever produced it.
func (v *Vision) scrub(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	if v.APIKey != "" {
		s = strings.ReplaceAll(s, v.APIKey, "<redacted>")
	}
	return errors.New(s)
}

// Read sends one image with one prompt and parses the answer strictly.
func (v *Vision) Read(ctx context.Context, img Image, prompt string) (Reading, error) {
	if !v.Enabled() {
		return Reading{}, errors.New("promo: vision is not configured")
	}
	timeout := v.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body := map[string]any{
		"model":       v.Model,
		"temperature": 0,
		"max_tokens":  600,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": prompt},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:" + img.ContentType + ";base64," + base64.StdEncoding.EncodeToString(img.Bytes),
				}},
			},
		}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Reading{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(v.BaseURL, "/")+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return Reading{}, v.scrub(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.APIKey)
	cl := v.Client
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return Reading{}, v.scrub(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Reading{}, v.scrub(err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is not echoed: it could carry anything.
		err := fmt.Errorf("promo: vision endpoint answered HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return Reading{}, fmt.Errorf("%w: %w", ErrVisionAuth, err)
		}
		if refusedForGood(resp.StatusCode) {
			return Reading{}, fmt.Errorf("%w: %w", ErrVisionRefused, err)
		}
		return Reading{}, err
	}
	var cc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &cc); err != nil || len(cc.Choices) == 0 {
		return Reading{}, ErrBadAnswer
	}
	return ParseReading(cc.Choices[0].Message.Content)
}

// ParseReading parses a model's answer: exactly one JSON object of the
// Reading shape, optionally inside one code fence. Unknown fields, trailing
// data, a missing products array or too many products refuse it.
func ParseReading(content string) (Reading, error) {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```JSON")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if !strings.HasPrefix(s, "{") || !exactKeys(s) {
		return Reading{}, ErrBadAnswer
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var r struct {
		Products *[]ReadProduct `json:"products"`
	}
	if err := dec.Decode(&r); err != nil || r.Products == nil {
		return Reading{}, ErrBadAnswer
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Reading{}, ErrBadAnswer
	}
	if len(*r.Products) > MaxReadProducts {
		return Reading{}, ErrBadAnswer
	}
	for _, p := range *r.Products {
		if !scalarOrNull(p.Price) || !scalarOrNull(p.WasPrice) {
			return Reading{}, ErrBadAnswer
		}
	}
	return Reading{Products: *r.Products}, nil
}

// productKeys are the only keys a product object may have.
var productKeys = map[string]bool{"title": true, "price": true, "was_price": true, "brand": true, "currency": true}

// exactKeys walks the answer's keys EXACTLY: encoding/json matches keys
// case-insensitively ("Price" fills price) and keeps the last of two equal
// keys, so a duplicated or re-cased key could make two readers disagree.
// The top level must be {"products":[{...},...]} with each key at most once.
func exactKeys(s string) bool {
	dec := json.NewDecoder(strings.NewReader(s))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	seenTop := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil || tok != "products" || seenTop {
			return false
		}
		seenTop = true
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return false
		}
		for dec.More() {
			if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
				return false
			}
			seen := map[string]bool{}
			for dec.More() {
				tok, err := dec.Token()
				k, ok := tok.(string)
				if err != nil || !ok || !productKeys[k] || seen[k] {
					return false
				}
				seen[k] = true
				var skip json.RawMessage
				if err := dec.Decode(&skip); err != nil {
					return false
				}
			}
			if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
				return false
			}
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
			return false
		}
	}
	tok, err := dec.Token()
	return err == nil && tok == json.Delim('}')
}

// scalarOrNull: a price may be a JSON string, number or null, nothing else.
func scalarOrNull(v any) bool {
	switch v.(type) {
	case nil, string, json.Number:
		return true
	}
	return false
}
