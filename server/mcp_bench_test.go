package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/baditaflorin/go-common/agent"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPReplayPreservesBodyAndErrorStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := &Server{Mux: http.NewServeMux()}
			body := "first\nsecond\x00last"
			s.Mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body[:5]))
				_, _ = w.Write([]byte(body[5:]))
			})
			result, err := mcpToolHandler(s, agent.Tool{})(context.Background(), &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{}})
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Content[0].(*sdkmcp.TextContent).Text; got != body {
				t.Fatalf("body=%q, want %q", got, body)
			}
			if result.IsError != (status >= 400) {
				t.Fatalf("IsError=%v for status %d", result.IsError, status)
			}
		})
	}
}

func BenchmarkMCPReplayResponse(b *testing.B) {
	for _, size := range []int{1024, 1 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			s := &Server{Mux: http.NewServeMux()}
			body := bytes.Repeat([]byte("x"), size)
			s.Mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) })
			h := mcpToolHandler(s, agent.Tool{})
			req := &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{}}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := h(context.Background(), req)
				if err != nil || result.IsError {
					b.Fatalf("result=%v err=%v", result, err)
				}
				if len(result.Content[0].(*sdkmcp.TextContent).Text) != size {
					b.Fatal("wrong response length")
				}
			}
		})
	}
}
