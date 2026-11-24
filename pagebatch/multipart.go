// Package pagebatch frames bounded groups of raw page requests and receipts.
package pagebatch

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strconv"

	"golang.org/x/net/http/httpguts"
)

const MaxParts = 8
const MaxRequestBytes = 8 << 20

var ErrRequestTooLarge = errors.New("pagebatch: request exceeds batch limits")
var ErrInvalidDocument = errors.New("pagebatch: invalid document")

type Document struct {
	Header textproto.MIMEHeader
	Body   []byte
}

type Receipt struct {
	Status int
	Body   []byte
}

// Encode assigns stable one-based correlation IDs for this request. The limit
// includes metadata and framing. Callers own the original immutable bodies.
func Encode(documents []Document) (string, []byte, error) {
	if len(documents) > MaxParts {
		return "", nil, ErrRequestTooLarge
	}
	if len(documents) == 0 {
		return "", nil, ErrInvalidDocument
	}
	var data bytes.Buffer
	writer := multipart.NewWriter(&data)
	for i, document := range documents {
		if len(document.Body) > MaxRequestBytes-data.Len() {
			return "", nil, ErrRequestTooLarge
		}
		headers := make(textproto.MIMEHeader, len(document.Header)+1)
		headerBytes := 0
		for k, values := range document.Header {
			if !httpguts.ValidHeaderFieldName(k) {
				return "", nil, ErrInvalidDocument
			}
			for _, value := range values {
				if len(k)+len(value)+4 > MaxRequestBytes-data.Len()-len(document.Body)-headerBytes {
					return "", nil, ErrRequestTooLarge
				}
				headerBytes += len(k) + len(value) + 4
				if !httpguts.ValidHeaderFieldValue(value) {
					return "", nil, ErrInvalidDocument
				}
				headers.Add(k, value)
			}
		}
		if len(headers.Values("Content-Transfer-Encoding")) > 0 {
			return "", nil, ErrInvalidDocument
		}
		headers.Set("Content-ID", strconv.Itoa(i+1))
		part, err := writer.CreatePart(headers)
		if err != nil {
			return "", nil, err
		}
		if _, err = part.Write(document.Body); err != nil {
			return "", nil, err
		}
		if data.Len() > MaxRequestBytes {
			return "", nil, ErrRequestTooLarge
		}
	}
	if err := writer.Close(); err != nil {
		return "", nil, err
	}
	if data.Len() > MaxRequestBytes {
		return "", nil, ErrRequestTooLarge
	}
	contentType := mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": writer.Boundary()})
	return contentType, data.Bytes(), nil
}

// Decode requires an independent reply for every input and restores request
// order. A partial or truncated reply cannot validate the affected request.
func Decode(contentType string, body io.Reader, count, maxPartBytes int) ([]Receipt, error) {
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != "multipart/mixed" || len(params["boundary"]) == 0 || len(params["boundary"]) > 70 ||
		count <= 0 || count > MaxParts || maxPartBytes <= 0 || maxPartBytes > MaxRequestBytes {
		return nil, errors.New("pagebatch: invalid batch response")
	}
	wire := &io.LimitedReader{R: body, N: int64(maxPartBytes*count) + (64 << 10) + 1}
	reader := multipart.NewReader(wire, params["boundary"])
	results := make([]Receipt, count)
	for range count {
		part, err := reader.NextRawPart()
		if err != nil {
			return nil, errors.New("pagebatch: missing part")
		}
		rawID := part.Header.Get("Content-ID")
		i, err := strconv.Atoi(rawID)
		rawStatus := part.Header.Get("X-Status-Code")
		status, statusErr := strconv.Atoi(rawStatus)
		if err != nil || i < 1 || i > count || strconv.Itoa(i) != rawID || results[i-1].Status != 0 ||
			len(part.Header.Values("Content-ID")) != 1 || len(part.Header.Values("X-Status-Code")) != 1 ||
			len(part.Header.Values("Content-Transfer-Encoding")) != 0 || statusErr != nil || strconv.Itoa(status) != rawStatus || status < 200 || status > 599 {
			return nil, errors.New("pagebatch: invalid part ID, encoding or status")
		}
		payload, err := io.ReadAll(io.LimitReader(part, int64(maxPartBytes)+1))
		if err != nil || len(payload) > maxPartBytes {
			return nil, errors.New("pagebatch: invalid part body")
		}
		results[i-1] = Receipt{Status: status, Body: payload}
	}
	if _, err := reader.NextRawPart(); err != io.EOF {
		return nil, errors.New("pagebatch: extra part or missing closing boundary")
	}
	if _, err := io.Copy(io.Discard, wire); err != nil || wire.N == 0 {
		return nil, errors.New("pagebatch: oversized or interrupted response")
	}
	return results, nil
}
