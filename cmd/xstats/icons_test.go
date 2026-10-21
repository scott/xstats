package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rgba8(c color.Color) (r, g, b, a uint8) {
	R, G, B, A := c.RGBA()
	return uint8(R >> 8), uint8(G >> 8), uint8(B >> 8), uint8(A >> 8)
}

func decodePNG(t *testing.T, b []byte) image.Image {
	t.Helper()
	im, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return im
}

func TestIconsArePublic(t *testing.T) {
	mux, _ := testDB(t)
	// No Authorization header. Icon routes are public static files.
	// /api/ingest is the route that requires the bearer token.
	cases := []struct {
		path  string
		ctype string
	}{
		{"/favicon.svg", "image/svg+xml"},
		{"/favicon.ico", "image/vnd.microsoft.icon"},
		{"/favicon-16.png", "image/png"},
		{"/favicon-32.png", "image/png"},
		{"/favicon-192.png", "image/png"},
		{"/favicon-512.png", "image/png"},
		{"/apple-touch-icon.png", "image/png"},
		{"/apple-touch-icon-precomposed.png", "image/png"},
		{"/site.webmanifest", "application/manifest+json"},
	}
	for _, c := range cases {
		w := get(mux, c.path)
		if w.Code != 200 {
			t.Fatalf("%s status %d", c.path, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != c.ctype {
			t.Fatalf("%s content-type %q", c.path, got)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=86400" {
			t.Fatalf("%s cache %q", c.path, got)
		}
	}

	// A wrong ingest token does not lock the icon. These files have no data.
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("icon with bogus bearer token: %d", rec.Code)
	}
	post := httptest.NewRecorder()
	mux.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/favicon.ico", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST favicon.ico: %d", post.Code)
	}

	svg := get(mux, "/favicon.svg").Body.String()
	for _, s := range []string{"#07090d", "#a371f7", "#1d9bf0", "#3d4a5c"} {
		if !strings.Contains(svg, s) {
			t.Fatalf("svg missing %s", s)
		}
	}
	if strings.Contains(svg, "href=") || strings.Contains(svg, "https://") || strings.Contains(svg, "url(") {
		t.Fatal("favicon.svg references an external file")
	}
	if !strings.Contains(svg, `xmlns="http://www.w3.org/2000/svg"`) {
		t.Fatal("favicon.svg missing svg namespace")
	}

	png16 := decodePNG(t, get(mux, "/favicon-16.png").Body.Bytes())
	if png16.Bounds().Dx() != 16 || png16.Bounds().Dy() != 16 {
		t.Fatalf("16 size %v", png16.Bounds())
	}
	// Solid 1px outline. Shrinking the 64px art leaves this pixel translucent.
	if r, g, b, a := rgba8(png16.At(8, 0)); r != 0x3d || g != 0x4a || b != 0x5c || a != 255 {
		t.Fatalf("16 outline pixel #%02x%02x%02x a=%d", r, g, b, a)
	}
	if r, g, b, a := rgba8(png16.At(4, 12)); r != 0xa3 || g != 0x71 || b != 0xf7 || a != 255 {
		t.Fatalf("16 bar pixel #%02x%02x%02x a=%d", r, g, b, a)
	}
	if _, _, _, a := rgba8(png16.At(0, 0)); a != 0 {
		t.Fatalf("16 corner alpha %d", a)
	}

	png32 := decodePNG(t, get(mux, "/favicon-32.png").Body.Bytes())
	if png32.Bounds().Dx() != 32 || png32.Bounds().Dy() != 32 {
		t.Fatalf("32 size %v", png32.Bounds())
	}
	if r, g, b, a := rgba8(png32.At(16, 0)); r != 0x3d || g != 0x4a || b != 0x5c || a != 255 {
		t.Fatalf("32 outline pixel #%02x%02x%02x a=%d", r, g, b, a)
	}
	for _, p := range []string{"/favicon-192.png", "/favicon-512.png"} {
		im := decodePNG(t, get(mux, p).Body.Bytes())
		want := 192
		if strings.Contains(p, "512") {
			want = 512
		}
		if im.Bounds().Dx() != want || im.Bounds().Dy() != want {
			t.Fatalf("%s size %v", p, im.Bounds())
		}
	}

	apple := decodePNG(t, get(mux, "/apple-touch-icon.png").Body.Bytes())
	pre := decodePNG(t, get(mux, "/apple-touch-icon-precomposed.png").Body.Bytes())
	if apple.Bounds().Dx() != 180 || apple.Bounds().Dy() != 180 {
		t.Fatalf("apple size %v", apple.Bounds())
	}
	// Full-bleed tile. iOS rounds the square, so the corner is opaque #07090d.
	if r, g, b, a := rgba8(apple.At(0, 0)); r != 0x07 || g != 0x09 || b != 0x0d || a != 255 {
		t.Fatalf("apple corner #%02x%02x%02x a=%d", r, g, b, a)
	}
	if !bytes.Equal(get(mux, "/apple-touch-icon.png").Body.Bytes(), get(mux, "/apple-touch-icon-precomposed.png").Body.Bytes()) {
		t.Fatal("precomposed alias differs")
	}
	_ = pre

	ico := get(mux, "/favicon.ico").Body.Bytes()
	if len(ico) < 6 || binary.LittleEndian.Uint16(ico[0:2]) != 0 || binary.LittleEndian.Uint16(ico[2:4]) != 1 {
		t.Fatal("bad ico header")
	}
	n := int(binary.LittleEndian.Uint16(ico[4:6]))
	if n != 3 {
		t.Fatalf("ico count %d", n)
	}
	wantSizes := map[int]bool{16: false, 32: false, 48: false}
	for i := 0; i < n; i++ {
		ent := ico[6+i*16 : 6+(i+1)*16]
		w := int(ent[0])
		h := int(ent[1])
		size := binary.LittleEndian.Uint32(ent[8:12])
		off := binary.LittleEndian.Uint32(ent[12:16])
		if w != h {
			t.Fatalf("ico entry %d is %dx%d", i, w, h)
		}
		if _, ok := wantSizes[w]; !ok {
			t.Fatalf("unexpected ico size %d", w)
		}
		wantSizes[w] = true
		blob := ico[off : off+size]
		cfg, err := png.DecodeConfig(bytes.NewReader(blob))
		if err != nil {
			t.Fatalf("ico %d: %v", w, err)
		}
		if cfg.Width != w || cfg.Height != h {
			t.Fatalf("ico %d png is %dx%d", w, cfg.Width, cfg.Height)
		}
	}
	for size, ok := range wantSizes {
		if !ok {
			t.Fatalf("ico missing %d", size)
		}
	}
	// The 16px image inside the ico is the tuned drawing, same outline pixel.
	ent0 := ico[6:22]
	off0 := binary.LittleEndian.Uint32(ent0[12:16])
	size0 := binary.LittleEndian.Uint32(ent0[8:12])
	im16 := decodePNG(t, ico[off0:off0+size0])
	if im16.Bounds().Dx() != 16 {
		t.Fatal("first ico image is not 16")
	}
	if r, g, b, a := rgba8(im16.At(8, 0)); r != 0x3d || g != 0x4a || b != 0x5c || a != 255 {
		t.Fatalf("ico 16 outline #%02x%02x%02x a=%d", r, g, b, a)
	}

	var manifest map[string]any
	if err := json.Unmarshal(get(mux, "/site.webmanifest").Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["theme_color"] != "#07090d" || manifest["background_color"] != "#07090d" {
		t.Fatalf("manifest colors %v %v", manifest["theme_color"], manifest["background_color"])
	}
	icons, _ := manifest["icons"].([]any)
	if len(icons) != 2 {
		t.Fatalf("manifest icons %d", len(icons))
	}

	page := get(mux, "/").Body.String()
	for _, s := range []string{
		`rel="icon" href="/favicon.svg"`,
		`href="/favicon-32.png" sizes="32x32"`,
		`href="/favicon-16.png" sizes="16x16"`,
		`href="/favicon.ico"`,
		`rel="apple-touch-icon" href="/apple-touch-icon.png"`,
		`rel="manifest" href="/site.webmanifest"`,
		`name="theme-color" content="#07090d"`,
	} {
		if !strings.Contains(page, s) {
			t.Fatalf("page missing %s", s)
		}
	}
}
