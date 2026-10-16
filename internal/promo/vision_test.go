package promo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseReading(t *testing.T) {
	ok := []string{
		`{"products":[]}`,
		"```json\n{\"products\":[{\"title\":\"A\",\"price\":\"1.00\",\"was_price\":null,\"brand\":null,\"currency\":null}]}\n```",
		`  {"products":[{"title":"A","price":1.5}]}  `,
	}
	for _, s := range ok {
		if _, err := ParseReading(s); err != nil {
			t.Errorf("ParseReading(%q): %v", s, err)
		}
	}
	bad := []string{
		``,
		`Sure! Here is the JSON: {"products":[]}`,
		`{"items":[]}`,
		`{}`,
		`{"products":null}`,
		`{"products":[]} {"products":[]}`,
		`{"products":[],"note":"x"}`,
		`{"products":[{"title":"A","price":"1","extra":true}]}`,
		`{"products":[{"title":"A","price":{"amount":1}}]}`,
		`{"products":[{"title":"A","price":["1"]}]}`,
		`{"products":[` + strings.Repeat(`{"title":"A","price":"1"},`, MaxReadProducts) + `{"title":"A","price":"1"}]}`,
		`[{"title":"A"}]`,
	}
	for _, s := range bad {
		if _, err := ParseReading(s); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("ParseReading(%q) = %v, want ErrBadAnswer", s, err)
		}
	}
}

func TestVisionRead(t *testing.T) {
	fv := newFakeVision(t)
	fv.set(7, answers{A: product("A 1TB Hard Drive", "10.00", "")})
	v := fv.vision()
	img := Image{Bytes: testImage(7, 100), ContentType: "image/png"}
	r, err := v.Read(context.Background(), img, PromptA)
	if err != nil || len(r.Products) != 1 || r.Products[0].Title != "A 1TB Hard Drive" {
		t.Fatalf("Read = %+v, %v", r, err)
	}
	if fv.keys[0] != "Bearer test-key-not-real" {
		t.Fatalf("auth header %q", fv.keys[0])
	}
	if (&Vision{BaseURL: "x", Model: "m"}).Enabled() || (*Vision)(nil).Enabled() {
		t.Fatal("a vision without a key is enabled")
	}
}

// An endpoint error never carries the key, whatever it echoes.
func TestVisionErrorsNeverCarryTheKey(t *testing.T) {
	const key = "sk-secret-value-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	defer srv.Close()
	v := &Vision{BaseURL: srv.URL + "/v1", APIKey: key, Model: "m", Client: srv.Client()}
	_, err := v.Read(context.Background(), Image{Bytes: []byte("x"), ContentType: "image/png"}, PromptA)
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("err = %v", err)
	}
	// A transport error naming the URL with the key in it is scrubbed too.
	v = &Vision{BaseURL: "http://bad host/" + key, APIKey: key, Model: "m"}
	_, err = v.Read(context.Background(), Image{Bytes: []byte("x"), ContentType: "image/png"}, PromptA)
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("transport err = %v", err)
	}
}
