package capturestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/baditaflorin/go-common/pagebatch"
)

func partsReader(format string, body io.Reader) (*multipart.Reader, error) {
	kind, params, err := mime.ParseMediaType(format)
	if err != nil || kind != "multipart/mixed" || params["boundary"] == "" {
		return nil, errors.New("expected multipart/mixed")
	}
	return multipart.NewReader(body, params["boundary"]), nil
}

func TestRawDerivationGroupKeepsExactJSONAndReceipts(t *testing.T) {
	items := []DerivationWrite{
		{Metadata: DerivationMetadata{IdempotencyKey: "compact"}, Body: []byte("{\"b\":true}")},
		{Metadata: DerivationMetadata{IdempotencyKey: "space"}, Body: []byte("{ \n \"text\": \"ș,é\" }\n")},
	}
	receipt := derivationBatchReceipt{}
	for i, item := range items {
		receipt.Results = append(receipt.Results, DerivationResult{Derivation: Derivation{
			ID: fmt.Sprintf("der_%d", i), CaptureID: "cap_x", DerivationMetadata: item.Metadata, BodyBytes: int64(len(item.Body)), GzipBytes: 9, SHA256: fmt.Sprintf("%x", sha256.Sum256(item.Body)),
		}})
	}
	var incorrectHash atomic.Bool
	c, server := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-scoped-key" {
			t.Error("missing scoped request")
			w.WriteHeader(401)
			return
		}
		grouped := r.URL.Path == "/v1/derivations/batch"
		raw, err := partsReader(r.Header.Get("Content-Type"), r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var responsePartID string
		if grouped {
			outer, err := raw.NextRawPart()
			if err != nil || outer.Header.Get("X-Capture-ID") != "cap_x" {
				t.Error("missing parent capture ID")
				w.WriteHeader(400)
				return
			}
			responsePartID = outer.Header.Get("Content-ID")
			raw, err = partsReader(outer.Header.Get("Content-Type"), outer)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
		} else if r.URL.Path != "/v1/captures/cap_x/derivations/batch-raw" {
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		for i, want := range items {
			part, err := raw.NextRawPart()
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			b, err := io.ReadAll(part)
			metadata, decodeErr := base64.RawURLEncoding.DecodeString(part.Header.Get("X-Derivation-Metadata"))
			var m DerivationMetadata
			if err != nil || decodeErr != nil || json.Unmarshal(metadata, &m) != nil || m != want.Metadata || !bytes.Equal(b, want.Body) ||
				part.Header.Get("Content-ID") != strconv.Itoa(i+1) || part.Header.Get("Content-Type") != "application/json" {
				t.Errorf("raw item %d changed", i)
				w.WriteHeader(400)
				return
			}
		}
		if _, err := raw.NextRawPart(); err != io.EOF {
			t.Error("incomplete group")
			w.WriteHeader(400)
			return
		}
		if !grouped {
			_ = json.NewEncoder(w).Encode(receipt)
			return
		}
		reply := multipart.NewWriter(w)
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+reply.Boundary())
		p, err := reply.CreatePart(textproto.MIMEHeader{"Content-ID": {responsePartID}, "X-Status-Code": {"200"}})
		if err != nil {
			t.Error(err)
			return
		}
		out := receipt
		if incorrectHash.Load() {
			out.Results = append([]DerivationResult(nil), receipt.Results...)
			out.Results[1].Derivation.SHA256 = out.Results[0].Derivation.SHA256
		}
		_ = json.NewEncoder(p).Encode(out)
		_ = reply.Close()
	}))
	defer server.Close()
	ctx := context.Background()
	got, err := c.CreateRawDerivationBatch(ctx, "cap_x", items)
	if err != nil || len(got) != 2 || got[1] != receipt.Results[1] {
		t.Fatalf("single group: %v, %v", got, err)
	}
	for _, mismatch := range []bool{false, true} {
		incorrectHash.Store(mismatch)
		got, err := c.CreateMultiCaptureDerivations(ctx, []DerivationGroup{{CaptureID: "cap_x", Items: items}}, true)
		if err != nil || len(got) != 1 || (got[0].Err != nil) != mismatch {
			t.Fatalf("group with mismatch=%t: %v, %v", mismatch, got, err)
		}
	}
}

func TestTypedOversizeIsBeforeAnyRequest(t *testing.T) {
	c, s := testClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("oversize operation reached service") }))
	defer s.Close()
	ctx := context.Background()
	raw := make([]byte, MaxBodyBytes+1)
	// Valid JSON below the raw cap; base64 plus metadata exceed the wire cap.
	base64Expanded := append(append([]byte{'"'}, bytes.Repeat([]byte{'x'}, MaxBodyBytes*3/4)...), '"')
	for _, tc := range []struct {
		raw  bool
		body []byte
	}{{false, base64Expanded}, {false, raw}, {true, raw}} {
		_, err := c.CreateMultiCaptureDerivations(ctx, []DerivationGroup{{CaptureID: "cap_x", Items: []DerivationWrite{{Body: tc.body}}}}, tc.raw)
		if !errors.Is(err, pagebatch.ErrRequestTooLarge) {
			t.Fatalf("raw=%t, size=%d: %v", tc.raw, len(tc.body), err)
		}
	}
	if _, err := c.CreateBatch(ctx, []CaptureWrite{{Body: raw}}); !errors.Is(err, pagebatch.ErrRequestTooLarge) {
		t.Fatalf("capture size: %v", err)
	}
	if _, err := c.CreateRawDerivationBatch(ctx, "cap_x", make([]DerivationWrite, pagebatch.MaxParts+1)); !errors.Is(err, pagebatch.ErrRequestTooLarge) {
		t.Fatalf("group count: %v", err)
	}
	if _, err := c.CreateMultiCaptureDerivations(ctx, []DerivationGroup{{CaptureID: "bad/id"}}, false); !errors.Is(err, pagebatch.ErrInvalidDocument) {
		t.Fatalf("invalid group: %v", err)
	}
}

func TestMultiCaptureBindsEachDigestAndPreservesPartialFailure(t *testing.T) {
	input := []CaptureWrite{
		{Metadata: Metadata{Namespace: "unit", IdempotencyKey: "a", SourceService: "unit", URL: "https://a.test", BodyKind: "website_response"}, Body: []byte{0, 255, 10}},
		{Metadata: Metadata{Namespace: "unit", IdempotencyKey: "b", SourceService: "unit", URL: "https://b.test", BodyKind: "website_response"}, Body: []byte("second\r\n")},
	}
	for i := range input {
		input[i].SHA256 = fmt.Sprintf("%x", sha256.Sum256(input[i].Body))
	}
	var secondStatus atomic.Int32
	var wrongDigest atomic.Bool
	c, s := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/captures/batch" || r.Header.Get("Authorization") != "Bearer test-scoped-key" {
			t.Error("unexpected capture batch request")
			w.WriteHeader(400)
			return
		}
		reader, err := partsReader(r.Header.Get("Content-Type"), r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		for i, item := range input {
			part, err := reader.NextRawPart()
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			got, err := io.ReadAll(part)
			encoded, decodeErr := base64.RawURLEncoding.DecodeString(part.Header.Get("X-Capture-Metadata"))
			var meta Metadata
			if err != nil || decodeErr != nil || json.Unmarshal(encoded, &meta) != nil || meta.IdempotencyKey != item.Metadata.IdempotencyKey || part.Header.Get("X-Source-SHA256") != item.SHA256 ||
				part.Header.Get("Content-ID") != strconv.Itoa(i+1) || !bytes.Equal(item.Body, got) {
				t.Errorf("capture %d lost input binding", i)
				w.WriteHeader(400)
				return
			}
		}
		replies := multipart.NewWriter(w)
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+replies.Boundary())
		for _, i := range []int{1, 0} {
			item := input[i]
			status := 200
			if i == 1 {
				status = int(secondStatus.Load())
			}
			p, err := replies.CreatePart(textproto.MIMEHeader{"Content-ID": {strconv.Itoa(i + 1)}, "X-Status-Code": {strconv.Itoa(status)}})
			if err != nil {
				t.Error(err)
				return
			}
			if status != 200 {
				_, _ = io.WriteString(p, "sensitive diagnostic")
				continue
			}
			digest := item.SHA256
			if i == 1 && wrongDigest.Load() {
				digest = input[0].SHA256
			}
			_ = json.NewEncoder(p).Encode(CreateResult{Capture: Capture{ID: fmt.Sprintf("cap_%d", i), Metadata: item.Metadata, SHA256: digest, BodyBytes: int64(len(item.Body)), GzipBytes: 24}})
		}
		_ = replies.Close()
	}))
	defer s.Close()
	for _, trial := range []struct {
		status int
		wrong  bool
	}{{200, false}, {409, false}, {200, true}} {
		secondStatus.Store(int32(trial.status))
		wrongDigest.Store(trial.wrong)
		got, err := c.CreateBatch(context.Background(), input)
		expectFailure := trial.status != 200 || trial.wrong
		if err != nil || len(got) != 2 || got[0].Err != nil || got[0].Result.Capture.ID != "cap_0" || (got[1].Err != nil) != expectFailure {
			t.Fatalf("case %+v: %v, %v", trial, got, err)
		}
	}
}
