package qrterm

import (
	"fmt"
	"net/http"

	qrcode "github.com/skip2/go-qrcode"
)

// FormatText and FormatPNG are the two renderings Write and QueryFormat
// know about. FormatText is terminal-friendly half-block art; FormatPNG
// is a 512x512 image/png.
const (
	FormatText = "text"
	FormatPNG  = "png"
)

// QueryFormat returns the QR format requested by r's "qr" query
// parameter: FormatPNG for "?qr=png", FormatText for any other present
// value ("1", "true", "text", or bare "?qr"), and "" if "qr" is absent
// entirely. Callers use the empty string to mean "no QR requested."
func QueryFormat(r *http.Request) string {
	v, ok := r.URL.Query()["qr"]
	if !ok {
		return ""
	}
	switch firstOr(v, "") {
	case FormatPNG:
		return FormatPNG
	default: // "1", "true", "text", "" → terminal-friendly text
		return FormatText
	}
}

// Write renders content as a QR code in the given format (FormatPNG or
// anything else, treated as FormatText) and writes it to w, setting the
// appropriate Content-Type and a no-store Cache-Control header. It
// returns an error instead of writing an HTTP error response itself —
// the caller owns its own error shape (most fleet services have their
// own httpError helper) and decides the status code and body. On error,
// Write has not written anything to w yet, so the caller's own error
// response is still safe to send.
func Write(w http.ResponseWriter, format, content string) error {
	switch format {
	case FormatPNG:
		png, err := PNG(content)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, err = w.Write(png)
		return err
	default:
		text, err := TextString(content)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, err = w.Write([]byte(text))
		return err
	}
}

// TextString returns half-block terminal art for content — a QR code
// that scans well from most terminal emulators and is copy-paste safe.
func TextString(content string) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", fmt.Errorf("qrterm: encode text: %w", err)
	}
	return q.ToSmallString(false), nil
}

// PNG returns a 512x512 image/png encoding of content.
func PNG(content string) ([]byte, error) {
	png, err := qrcode.Encode(content, qrcode.Medium, 512)
	if err != nil {
		return nil, fmt.Errorf("qrterm: encode png: %w", err)
	}
	return png, nil
}

func firstOr(ss []string, def string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return def
}
