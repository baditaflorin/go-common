package qrterm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueryFormat(t *testing.T) {
	cases := map[string]string{
		"https://pipe.0exec.com/x":          "",     // no ?qr at all
		"https://pipe.0exec.com/x?qr":       "text", // bare ?qr
		"https://pipe.0exec.com/x?qr=1":     "text",
		"https://pipe.0exec.com/x?qr=true":  "text",
		"https://pipe.0exec.com/x?qr=text":  "text",
		"https://pipe.0exec.com/x?qr=png":   "png",
		"https://pipe.0exec.com/x?other=1":  "",
		"https://pipe.0exec.com/x?qr=&y=1":  "text",
		"https://pipe.0exec.com/x?qr=weird": "text", // unrecognized value falls back to text
	}
	for rawURL, want := range cases {
		r := httptest.NewRequest(http.MethodGet, rawURL, nil)
		if got := QueryFormat(r); got != want {
			t.Errorf("QueryFormat(%q) = %q, want %q", rawURL, got, want)
		}
	}
}

func TestTextString(t *testing.T) {
	got, err := TextString("https://pipe.0exec.com/abc123")
	if err != nil {
		t.Fatalf("TextString: unexpected error: %v", err)
	}
	if got == "" {
		t.Fatal("TextString: got empty art for valid content")
	}
	// Half-block art is multi-line QR output, not a plain echo of content.
	if strings.Contains(got, "https://") {
		t.Errorf("TextString: expected rendered QR art, got what looks like raw content: %q", got)
	}
}

func TestTextString_TooLong(t *testing.T) {
	// Exceeds the QR spec's max byte-mode capacity at any recovery level
	// (the largest, version 40 / Low, holds 2953 bytes), so encoding
	// must fail.
	huge := strings.Repeat("x", 4000)
	if _, err := TextString(huge); err == nil {
		t.Fatal("TextString: expected error for oversized content, got nil")
	}
}

func TestPNG(t *testing.T) {
	png, err := PNG("https://pipe.0exec.com/abc123")
	if err != nil {
		t.Fatalf("PNG: unexpected error: %v", err)
	}
	if len(png) == 0 {
		t.Fatal("PNG: got empty image for valid content")
	}
	// PNG signature: 89 50 4E 47 0D 0A 1A 0A
	sig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if len(png) < len(sig) || string(png[:len(sig)]) != string(sig) {
		t.Error("PNG: output does not start with the PNG magic bytes")
	}
}

func TestPNG_TooLong(t *testing.T) {
	huge := strings.Repeat("x", 4000)
	if _, err := PNG(huge); err == nil {
		t.Fatal("PNG: expected error for oversized content, got nil")
	}
}

func TestWrite_Text(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Write(rec, FormatText, "https://pipe.0exec.com/abc123"); err != nil {
		t.Fatalf("Write(text): unexpected error: %v", err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Write(text): Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Write(text): Cache-Control = %q, want no-store", cc)
	}
	if rec.Body.Len() == 0 {
		t.Error("Write(text): empty body")
	}
}

func TestWrite_PNG(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Write(rec, FormatPNG, "https://pipe.0exec.com/abc123"); err != nil {
		t.Fatalf("Write(png): unexpected error: %v", err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Write(png): Content-Type = %q, want image/png", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Write(png): Cache-Control = %q, want no-store", cc)
	}
	if rec.Body.Len() == 0 {
		t.Error("Write(png): empty body")
	}
}

func TestWrite_DefaultFallsBackToText(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Write(rec, "", "https://pipe.0exec.com/abc123"); err != nil {
		t.Fatalf("Write(\"\"): unexpected error: %v", err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Write(\"\"): Content-Type = %q, want text/plain; charset=utf-8 (default format)", ct)
	}
}

func TestWrite_ErrorDoesNotWriteBody(t *testing.T) {
	huge := strings.Repeat("x", 4000)
	rec := httptest.NewRecorder()
	if err := Write(rec, FormatPNG, huge); err == nil {
		t.Fatal("Write(png, oversized): expected error, got nil")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("Write: expected no body written on encode error, got %d bytes", rec.Body.Len())
	}
	if rec.Header().Get("Content-Type") != "" {
		t.Errorf("Write: expected no Content-Type set on encode error, got %q", rec.Header().Get("Content-Type"))
	}
}
