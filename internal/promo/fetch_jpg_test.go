package promo

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"net/http"
	"testing"
	"time"
)

// image.richrelevance.com answers "image/jpg" for real JPEGs: the declared
// spelling is normalised, the sniff still decides.
func TestFetchImageNonStandardJPEGType(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 120, 120))
	r := rand.New(rand.NewSource(1))
	for y := 0; y < 120; y++ {
		for x := 0; x < 120; x++ {
			img.Set(x, y, color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255})
		}
	}
	var jb bytes.Buffer
	if err := jpeg.Encode(&jb, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	w.handle("img.example.com", func(rw http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/jpg":
			rw.Header().Set("Content-Type", "image/jpg")
			_, _ = rw.Write(jb.Bytes())
		case "/pjpeg":
			rw.Header().Set("Content-Type", "image/pjpeg; charset=binary")
			_, _ = rw.Write(jb.Bytes())
		case "/jpg-but-html":
			rw.Header().Set("Content-Type", "image/jpg")
			_, _ = rw.Write(bytes.Repeat([]byte("<html><body>x</body></html>"), 200))
		}
	})
	f := &Fetcher{Client: w.client(), Backoff: func(int) time.Duration { return 0 }}
	for _, p := range []string{"/jpg", "/pjpeg"} {
		got, err := f.FetchImage(context.Background(), "https://img.example.com"+p, []string{"img.example.com"})
		if err != nil || got.ContentType != "image/jpeg" {
			t.Fatalf("%s: %v %q", p, err, got.ContentType)
		}
	}
	if _, err := f.FetchImage(context.Background(), "https://img.example.com/jpg-but-html", []string{"img.example.com"}); fetchErr(t, err).Reason != DropNotImage {
		t.Fatalf("a non-image declared image/jpg was accepted: %v", err)
	}
	for in, want := range map[string]string{"image/jpg": "image/jpeg", "IMAGE/PJPEG": "image/jpeg", "image/png; x=1": "image/png", "text/html": "text/html"} {
		if got := normImageType(in); got != want {
			t.Errorf("normImageType(%q) = %q", in, got)
		}
	}
}
