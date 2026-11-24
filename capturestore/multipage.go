package capturestore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/baditaflorin/go-common/pagebatch"
)

// CaptureWrite binds already-hashed raw bytes to their capture metadata.
type CaptureWrite struct {
	Metadata Metadata
	Body     []byte
	SHA256   string
}

type CaptureWriteResult struct {
	Result CreateResult
	Err    error
}

func (c *Client) CreateBatch(ctx context.Context, pages []CaptureWrite) ([]CaptureWriteResult, error) {
	if len(pages) > pagebatch.MaxParts {
		return nil, pagebatch.ErrRequestTooLarge
	}
	documents := make([]pagebatch.Document, len(pages))
	for i, page := range pages {
		if len(page.Body) > MaxBodyBytes {
			return nil, pagebatch.ErrRequestTooLarge
		}
		if !shaPattern.MatchString(page.SHA256) || page.SHA256 != strings.ToLower(page.SHA256) {
			return nil, fmt.Errorf("capturestore: invalid source digest: %w", pagebatch.ErrInvalidDocument)
		}
		headers, err := captureUploadHeaders(page.Metadata, len(page.Body), page.SHA256)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", err, pagebatch.ErrInvalidDocument)
		}
		headers.Set("Content-Type", "application/octet-stream")
		documents[i] = pagebatch.Document{Header: textproto.MIMEHeader(headers), Body: page.Body}
	}
	receipts, err := c.postPageBatch(ctx, capturesPath+"/batch", documents)
	if err != nil {
		return nil, err
	}
	results := make([]CaptureWriteResult, len(pages))
	for i, receipt := range receipts {
		page := pages[i]
		if receipt.Status != http.StatusOK {
			results[i].Err = fmt.Errorf("capturestore: server returned HTTP %d", receipt.Status)
		} else if json.Unmarshal(receipt.Body, &results[i].Result) != nil ||
			!validCaptureReceipt(results[i].Result.Capture, page.Metadata, len(page.Body), page.SHA256) {
			results[i] = CaptureWriteResult{Err: errors.New("capturestore: invalid create receipt")}
		}
	}
	return results, nil
}

type DerivationGroup struct {
	CaptureID string
	Items     []DerivationWrite
}

type DerivationGroupResult struct {
	Results []DerivationResult
	Err     error
}

// CreateRawDerivationBatch commits the same group as CreateDerivationBatch
// while sending each parser output's exact bytes in its own MIME part.
func (c *Client) CreateRawDerivationBatch(ctx context.Context, captureID string, items []DerivationWrite) ([]DerivationResult, error) {
	format, data, err := rawDerivationBatch(DerivationGroup{CaptureID: captureID, Items: items})
	if err != nil {
		return nil, err
	}
	var reply derivationBatchReceipt
	if _, err = c.do(ctx, http.MethodPost, capturesPath+"/"+captureID+"/derivations/batch-raw", nil, data, format, nil, &reply); err != nil {
		return nil, err
	}
	return validateDerivationBatch(captureID, items, reply)
}

func (c *Client) CreateMultiCaptureDerivations(ctx context.Context, groups []DerivationGroup, raw bool) ([]DerivationGroupResult, error) {
	if len(groups) > pagebatch.MaxParts {
		return nil, pagebatch.ErrRequestTooLarge
	}
	documents := make([]pagebatch.Document, len(groups))
	for i, group := range groups {
		var format string
		var data []byte
		var err error
		if raw {
			format, data, err = rawDerivationBatch(group)
		} else {
			format = "application/json"
			data, err = encodeDerivationBatch(group.CaptureID, group.Items)
		}
		if err != nil {
			if errors.Is(err, errDerivationBatchTooLarge) || errors.Is(err, errDerivationRawBytesTooLarge) {
				return nil, fmt.Errorf("%s: %w", err, pagebatch.ErrRequestTooLarge)
			}
			if raw {
				return nil, err
			}
			return nil, fmt.Errorf("%s: %w", err, pagebatch.ErrInvalidDocument)
		}
		documents[i] = pagebatch.Document{Header: textproto.MIMEHeader{
			"Content-Type": {format}, "X-Capture-ID": {group.CaptureID},
		}, Body: data}
	}
	receipts, err := c.postPageBatch(ctx, "/v1/derivations/batch", documents)
	if err != nil {
		return nil, err
	}
	results := make([]DerivationGroupResult, len(groups))
	for i, receipt := range receipts {
		group := groups[i]
		var reply derivationBatchReceipt
		if receipt.Status != http.StatusOK {
			results[i].Err = fmt.Errorf("capturestore: server returned HTTP %d", receipt.Status)
		} else if json.Unmarshal(receipt.Body, &reply) != nil {
			results[i].Err = errors.New("capturestore: invalid derivation batch response")
		} else {
			results[i].Results, results[i].Err = validateDerivationBatch(group.CaptureID, group.Items, reply)
		}
	}
	return results, nil
}

func rawDerivationBatch(group DerivationGroup) (string, []byte, error) {
	if len(group.Items) > pagebatch.MaxParts {
		return "", nil, pagebatch.ErrRequestTooLarge
	}
	if !validID(group.CaptureID) || len(group.Items) == 0 {
		return "", nil, fmt.Errorf("capturestore: invalid derivation batch: %w", pagebatch.ErrInvalidDocument)
	}
	documents := make([]pagebatch.Document, len(group.Items))
	for i, item := range group.Items {
		metadata, err := json.Marshal(item.Metadata)
		if err != nil {
			return "", nil, fmt.Errorf("capturestore: invalid derivation metadata: %w", pagebatch.ErrInvalidDocument)
		}
		encoded := base64.RawURLEncoding.EncodeToString(metadata)
		if len(encoded) > 4096 {
			return "", nil, fmt.Errorf("capturestore: derivation metadata exceeds limit: %w", pagebatch.ErrInvalidDocument)
		}
		documents[i] = pagebatch.Document{Header: textproto.MIMEHeader{
			"Content-Type": {"application/json"}, "X-Derivation-Metadata": {encoded},
		}, Body: item.Body}
	}
	return pagebatch.Encode(documents)
}

func (c *Client) postPageBatch(ctx context.Context, path string, documents []pagebatch.Document) ([]pagebatch.Receipt, error) {
	format, data, err := pagebatch.Encode(documents)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, path, nil, data, format, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return pagebatch.Decode(resp.Header.Get("Content-Type"), resp.Body, len(documents), maxResponseBytes)
}
