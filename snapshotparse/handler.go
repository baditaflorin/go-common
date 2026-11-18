// Package snapshotparse serves deterministic extraction from saved HTML with
// a receipt for the exact input bytes. Owners provide the parser and auth.
package snapshotparse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

const (
	SchemaVersion = "snapshot.parse.v1"
	MaxBytes      = 8 << 20
	Parsed        = "parsed"
	Empty         = "empty"
	NotApplicable = "not_applicable"
)

var captureID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Input contains verified, unmodified HTML bytes and their recorded provenance.
// URL is for evidence and relative link resolution. The handler never fetches it.
type Input struct {
	HTML        []byte
	URL         string
	SHA256      string
	CaptureID   string
	ContentType string
}

// UTF8 returns an HTML-decoded view for text-oriented parsers. The original
// bytes in HTML and the checksum in the receipt remain unchanged.
func (in Input) UTF8() ([]byte, error) {
	reader, err := charset.NewReader(bytes.NewReader(in.HTML), in.ContentType)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(reader, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBytes || !utf8.Valid(body) {
		return nil, errors.New("snapshot could not be decoded within the UTF-8 limit")
	}
	return body, nil
}

// Result holds deterministic signals. Empty means the supplied page had no
// supported signals. Use NotApplicable when the input cannot support the check.
type Result struct {
	Status     string          `json:"status"`
	Data       any             `json:"data"`
	Warnings   []string        `json:"warnings,omitempty"`
	Truncation map[string]bool `json:"truncation,omitempty"`
}

// Parser inspects only the supplied bytes. It should honor cancellation and
// bound its own work. Errors are reported without disclosing parser internals.
type Parser func(context.Context, Input) (Result, error)

type receipt struct {
	SchemaVersion string            `json:"schema_version"`
	Parser        map[string]string `json:"parser"`
	Input         map[string]string `json:"input"`
	Result
}

// Handler accepts POST with HTML or XHTML, X-Source-URL, X-Source-SHA256 and
// X-Capture-ID. Mount it behind the owning service's existing authorization.
func Handler(id, version string, parse Parser) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		input, code := readInput(w, r)
		if code != 0 {
			http.Error(w, http.StatusText(code), code)
			return
		}
		result, err := parse(r.Context(), input)
		if err != nil {
			http.Error(w, "snapshot parsing failed", http.StatusUnprocessableEntity)
			return
		}
		if result.Data == nil || (result.Status != Parsed && result.Status != Empty && result.Status != NotApplicable) {
			http.Error(w, "invalid snapshot result", http.StatusInternalServerError)
			return
		}
		body, err := json.Marshal(receipt{
			SchemaVersion: SchemaVersion,
			Parser:        map[string]string{"id": id, "version": version},
			Input:         map[string]string{"url": input.URL, "sha256": input.SHA256, "capture_id": input.CaptureID},
			Result:        result,
		})
		if err != nil || len(body) > MaxBytes {
			http.Error(w, "snapshot result unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	})
}

func readInput(w http.ResponseWriter, r *http.Request) (Input, int) {
	input := Input{
		URL:         r.Header.Get("X-Source-URL"),
		SHA256:      r.Header.Get("X-Source-SHA256"),
		CaptureID:   r.Header.Get("X-Capture-ID"),
		ContentType: r.Header.Get("Content-Type"),
	}
	media, _, err := mime.ParseMediaType(input.ContentType)
	if err != nil || (media != "text/html" && media != "application/xhtml+xml") || r.Header.Get("Content-Encoding") != "" {
		return Input{}, http.StatusUnsupportedMediaType
	}
	u, err := url.Parse(input.URL)
	if err != nil || len(input.URL) > 8192 || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || !captureID.MatchString(input.CaptureID) {
		return Input{}, http.StatusBadRequest
	}
	claimed, err := hex.DecodeString(input.SHA256)
	if err != nil || len(claimed) != sha256.Size {
		return Input{}, http.StatusBadRequest
	}
	input.HTML, err = io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return Input{}, http.StatusRequestEntityTooLarge
	}
	if err != nil || len(input.HTML) == 0 {
		return Input{}, http.StatusBadRequest
	}
	digest := sha256.Sum256(input.HTML)
	if !bytes.Equal(claimed, digest[:]) || input.SHA256 != hex.EncodeToString(digest[:]) {
		return Input{}, http.StatusUnprocessableEntity
	}
	return input, 0
}
