package capturestore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

const (
	MaxDerivationBatchItems = 24
	MaxDerivationBatchBytes = 8 << 20
)

var errDerivationBatchTooLarge = errors.New("capturestore: derivation batch exceeds wire limit")
var errDerivationRawBytesTooLarge = errors.New("capturestore: invalid derivation batch payload")

// DerivationWrite keeps the exact owner response bytes. The JSON wire format
// uses base64 rather than re-encoding the parser's JSON object.
type DerivationWrite struct {
	Metadata DerivationMetadata `json:"metadata"`
	Body     []byte             `json:"body"`
}

type DerivationBatch struct {
	Items []DerivationWrite `json:"items"`
}

// CreateDerivationBatch commits results for one capture and returns one
// checksum-verified receipt per input in request order.
func (c *Client) CreateDerivationBatch(ctx context.Context, captureID string, items []DerivationWrite) ([]DerivationResult, error) {
	body, err := encodeDerivationBatch(captureID, items)
	if err != nil {
		return nil, err
	}
	var out derivationBatchReceipt
	_, err = c.do(ctx, http.MethodPost, capturesPath+"/"+url.PathEscape(captureID)+"/derivations/batch", nil, body, "application/json", nil, &out)
	if err != nil {
		return nil, err
	}
	return validateDerivationBatch(captureID, items, out)
}

type derivationBatchReceipt struct {
	Results []DerivationResult `json:"results"`
}

func encodeDerivationBatch(captureID string, items []DerivationWrite) ([]byte, error) {
	if !validID(captureID) || len(items) == 0 || len(items) > MaxDerivationBatchItems {
		return nil, errors.New("capturestore: invalid derivation batch")
	}
	var rawBytes int
	for _, item := range items {
		rawBytes += len(item.Body)
		if rawBytes > MaxDerivationBatchBytes {
			return nil, errDerivationRawBytesTooLarge
		}
		if !json.Valid(item.Body) {
			return nil, errors.New("capturestore: invalid derivation batch payload")
		}
	}
	body, err := json.Marshal(DerivationBatch{Items: items})
	if err != nil {
		return nil, errors.New("capturestore: derivation batch exceeds wire limit")
	}
	if len(body) > MaxDerivationBatchBytes {
		return nil, errDerivationBatchTooLarge
	}
	return body, nil
}

func validateDerivationBatch(captureID string, items []DerivationWrite, out derivationBatchReceipt) ([]DerivationResult, error) {
	if len(out.Results) != len(items) {
		return nil, errors.New("capturestore: invalid derivation batch receipt count")
	}
	for i, result := range out.Results {
		if !validDerivationReceipt(result.Derivation, captureID, items[i].Metadata, items[i].Body) {
			return nil, errors.New("capturestore: invalid derivation batch receipt")
		}
	}
	return out.Results, nil
}
