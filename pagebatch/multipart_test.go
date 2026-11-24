package pagebatch

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"
	"testing"
)

func TestExactReceiptCorrelationAndFraming(t *testing.T) {
	input := []Document{
		{Header: textproto.MIMEHeader{"Content-Type": {"text/plain"}}, Body: []byte("one\r\n\r\n")},
		{Body: []byte{0, 0xfe, '=', '\r', '\n'}},
	}
	format, data, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(format)
	if err != nil {
		t.Fatal(err)
	}
	request := multipart.NewReader(bytes.NewReader(data), params["boundary"])
	for i, expected := range input {
		part, err := request.NextRawPart()
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(part)
		if err != nil || !bytes.Equal(actual, expected.Body) || part.Header.Get("Content-ID") != []string{"1", "2"}[i] {
			t.Fatalf("request part %d changed", i)
		}
	}
	if _, err := request.NextRawPart(); err != io.EOF {
		t.Fatalf("request terminator: %v", err)
	}
	if input[0].Header.Get("Content-ID") != "" {
		t.Fatal("input header mutated")
	}

	first := Document{Header: textproto.MIMEHeader{"Content-ID": {"2"}, "X-Status-Code": {"409"}}, Body: input[1].Body}
	second := Document{Header: textproto.MIMEHeader{"Content-ID": {"1"}, "X-Status-Code": {"200"}}, Body: input[0].Body}
	encodeReply := func(parts ...Document) []byte {
		t.Helper()
		var b bytes.Buffer
		m := multipart.NewWriter(&b)
		if err := m.SetBoundary("receipt"); err != nil {
			t.Fatal(err)
		}
		for _, doc := range parts {
			p, err := m.CreatePart(doc.Header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Write(doc.Body); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	decode := func(wire []byte) ([]Receipt, error) {
		return Decode("multipart/mixed; boundary=receipt", bytes.NewReader(wire), 2, 20)
	}
	wire := encodeReply(first, second)
	got, err := decode(wire)
	if err != nil || got[0].Status != 200 || got[1].Status != 409 || !bytes.Equal(got[0].Body, input[0].Body) || !bytes.Equal(got[1].Body, input[1].Body) {
		t.Fatalf("reply lost exact bytes, correlation or status: %v %v", got, err)
	}
	for name, bad := range map[string][]byte{
		"missing": encodeReply(first), "duplicate": encodeReply(first, first),
		"extra": encodeReply(first, second, second), "truncated": wire[:len(wire)-7],
		"oversized part":   encodeReply(first, Document{Header: second.Header, Body: make([]byte, 21)}),
		"noncanonical ID":  bytes.Replace(wire, []byte("Content-ID: 2"), []byte("Content-ID: 02"), 1),
		"encoded transfer": bytes.Replace(wire, []byte("X-Status-Code: 409"), []byte("Content-Transfer-Encoding: base64\r\nX-Status-Code: 409"), 1),
		"status":           bytes.Replace(wire, []byte("X-Status-Code: 409"), []byte("X-Status-Code: +409"), 1),
		"epilogue":         append(append([]byte{}, wire...), make([]byte, 70<<10)...),
	} {
		if _, err := decode(bad); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
}

func TestPreflightWireLimits(t *testing.T) {
	for _, docs := range [][]Document{
		make([]Document, MaxParts+1),
		{{Body: make([]byte, MaxRequestBytes)}},
		{{Header: textproto.MIMEHeader{"X-Large": {strings.Repeat("a", MaxRequestBytes)}}}},
	} {
		if _, _, err := Encode(docs); !errors.Is(err, ErrRequestTooLarge) {
			t.Fatalf("size error: %v", err)
		}
	}
	for _, docs := range [][]Document{nil, {{Header: textproto.MIMEHeader{"X-Bad": {"x\r\nX-Abc: y"}}}}, {{Header: textproto.MIMEHeader{"Content-Transfer-Encoding": {"base64"}}}}} {
		if _, _, err := Encode(docs); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("invalid data error: %v", err)
		}
	}
}
