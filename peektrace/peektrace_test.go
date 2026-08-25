package peektrace

import (
	"context"
	"net/http"
	"testing"
)

func TestTraceRoundTrip(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set(IDHeader, id)
	h.Set(DomainHeader, "https://Example.COM/")
	trace, ok := FromHeaders(h)
	if !ok || trace.ID != id || trace.Domain != "example.com" {
		t.Fatalf("unexpected trace: %+v, ok=%v", trace, ok)
	}
	ctx := WithTrace(context.Background(), trace)
	got, ok := FromContext(ctx)
	if !ok || got != trace {
		t.Fatalf("context trace mismatch: %+v, ok=%v", got, ok)
	}
}

func TestInvalidTraceIsIgnored(t *testing.T) {
	h := http.Header{}
	h.Set(IDHeader, "not-a-trace")
	h.Set(DomainHeader, "example.com")
	if _, ok := FromHeaders(h); ok {
		t.Fatal("invalid trace accepted")
	}
}

func TestHostInScope(t *testing.T) {
	for _, target := range []string{"https://example.com/ads.txt", "https://www.example.com/llms.txt"} {
		if !HostInScope("example.com", target) {
			t.Errorf("%q should be in scope", target)
		}
	}
	if HostInScope("example.com", "https://example.org/") {
		t.Fatal("different host reported in scope")
	}
}
