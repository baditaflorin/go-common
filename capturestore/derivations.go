package capturestore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// DerivationMetadata binds one immutable parser result to the exact source bytes.
type DerivationMetadata struct {
	IdempotencyKey string `json:"idempotency_key"`
	InputSHA256    string `json:"input_sha256"`
	ParserID       string `json:"parser_id"`
	ParserVersion  string `json:"parser_version"`
	OptionsSHA256  string `json:"options_sha256"`
	Status         string `json:"status"`
}
type Derivation struct {
	ID string `json:"id"`
	DerivationMetadata
	CaptureID string `json:"capture_id"`
	Namespace string `json:"namespace"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	BodyBytes int64  `json:"body_bytes"`
	GzipBytes int64  `json:"gzip_bytes"`
	CreatedAt string `json:"created_at"`
}
type DerivationResult struct {
	Derivation Derivation `json:"derivation"`
	Reused     bool       `json:"reused"`
}
type DerivationListOptions struct {
	Namespace, Domain, CaptureID, ParserID string
	Limit, Offset                          int
}
type DerivationList struct {
	Derivations []Derivation `json:"derivations"`
	Total       int64        `json:"total"`
	Limit       int          `json:"limit"`
	Offset      int          `json:"offset"`
}

func (c *Client) CreateDerivation(ctx context.Context, captureID string, m DerivationMetadata, body json.RawMessage) (DerivationResult, error) {
	var out DerivationResult
	if !validID(captureID) || len(body) > MaxBodyBytes || !json.Valid(body) || !shaPattern.MatchString(m.InputSHA256) || !shaPattern.MatchString(m.OptionsSHA256) || m.IdempotencyKey == "" || m.ParserID == "" || m.ParserVersion == "" {
		return out, errors.New("capturestore: invalid derivation input")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return out, errors.New("capturestore: invalid derivation metadata")
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > 4096 {
		return out, errors.New("capturestore: derivation metadata exceeds limit")
	}
	headers := make(http.Header)
	headers.Set("X-Derivation-Metadata", encoded)
	_, err = c.do(ctx, http.MethodPost, capturesPath+"/"+url.PathEscape(captureID)+"/derivations", nil, body, "application/json", headers, &out)
	if err != nil {
		return out, err
	}
	if !validDerivationReceipt(out.Derivation, captureID, m, body) {
		return DerivationResult{}, errors.New("capturestore: invalid derivation receipt")
	}
	return out, nil
}

func validDerivationReceipt(d Derivation, captureID string, m DerivationMetadata, body []byte) bool {
	sum := sha256.Sum256(body)
	return validID(d.ID) && d.CaptureID == captureID && d.DerivationMetadata == m &&
		d.BodyBytes == int64(len(body)) && d.SHA256 == hex.EncodeToString(sum[:]) && d.GzipBytes > 0
}
func (c *Client) ListDerivations(ctx context.Context, o DerivationListOptions) (DerivationList, error) {
	var out DerivationList
	if o.Limit < 0 || o.Limit > 100 || o.Offset < 0 {
		return out, errors.New("capturestore: invalid derivation pagination")
	}
	q := url.Values{}
	for k, v := range map[string]string{"namespace": o.Namespace, "domain": o.Domain, "capture_id": o.CaptureID, "parser_id": o.ParserID} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if o.Limit > 0 {
		q.Set("limit", fmt.Sprint(o.Limit))
	}
	if o.Offset > 0 {
		q.Set("offset", fmt.Sprint(o.Offset))
	}
	_, err := c.do(ctx, http.MethodGet, "/v1/derivations", q, nil, "", nil, &out)
	return out, err
}
func (c *Client) GetDerivation(ctx context.Context, id string) (Derivation, error) {
	var out struct {
		Derivation Derivation `json:"derivation"`
	}
	if !validID(id) {
		return out.Derivation, errors.New("capturestore: invalid derivation ID")
	}
	_, err := c.do(ctx, http.MethodGet, "/v1/derivations/"+url.PathEscape(id), nil, nil, "", nil, &out)
	if err == nil && out.Derivation.ID != id {
		return Derivation{}, errors.New("capturestore: invalid derivation receipt")
	}
	return out.Derivation, err
}

// DerivationBody verifies both the response checksum and the previously read
// immutable metadata. The JSON is data; callers must never render it as HTML.
func (c *Client) DerivationBody(ctx context.Context, d Derivation) (json.RawMessage, error) {
	if !validID(d.ID) || !shaPattern.MatchString(d.SHA256) || d.BodyBytes < 0 || d.BodyBytes > MaxBodyBytes {
		return nil, errors.New("capturestore: invalid derivation metadata")
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/derivations/"+url.PathEscape(d.ID)+"/body", nil, nil, "", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("X-Derivation-SHA256") != d.SHA256 {
		return nil, errors.New("capturestore: invalid derivation body receipt")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil || int64(len(body)) != d.BodyBytes || len(body) > MaxBodyBytes || !json.Valid(body) {
		return nil, errors.New("capturestore: invalid derivation body")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != d.SHA256 {
		return nil, errors.New("capturestore: derivation checksum mismatch")
	}
	return body, nil
}
