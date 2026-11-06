package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/agent"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPResponseLimit(t *testing.T) {
	for _, size := range []int{15, 16, 17} {
		for _, stringWriter := range []bool{false, true} {
			t.Run(fmt.Sprintf("bytes=%d/string=%v", size, stringWriter), func(t *testing.T) {
				s := &Server{Mux: http.NewServeMux()}
				WithMCPResponseLimit(16)(s)
				s.Mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
					if stringWriter {
						_, _ = io.WriteString(w, strings.Repeat("x", size))
					} else {
						_, _ = w.Write([]byte(strings.Repeat("x", size)))
					}
				})
				result, err := mcpToolHandler(s, agent.Tool{})(context.Background(), &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{}})
				if err != nil {
					t.Fatal(err)
				}
				if result.IsError != (size > 16) {
					t.Fatalf("IsError=%v, size=%d", result.IsError, size)
				}
				if size <= 16 && result.Content[0].(*sdkmcp.TextContent).Text != strings.Repeat("x", size) {
					t.Fatal("response changed below limit")
				}
			})
		}
	}
}

func TestMCPRecorderRejectsRepeatedOverflowWithoutRetainingBody(t *testing.T) {
	w := &mcpResponseRecorder{ResponseRecorder: httptest.NewRecorder(), ctx: context.Background(), limit: 16}
	for i := 0; i < 1000; i++ {
		if _, err := io.WriteString(w, strings.Repeat("x", 17)); err != errMCPResponseLimit {
			t.Fatalf("error=%v", err)
		}
	}
	if w.Body.Len() != 0 {
		t.Fatalf("buffered %d bytes after overflow", w.Body.Len())
	}
}

func TestMCPCancellationPreventsReplayAndFurtherBuffering(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Server{Mux: http.NewServeMux()}
	s.Mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) { t.Fatal("cancelled tool replayed") })
	result, err := mcpToolHandler(s, agent.Tool{})(ctx, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{}})
	if err != nil || !result.IsError {
		t.Fatalf("result=%v err=%v", result, err)
	}
	w := &mcpResponseRecorder{ResponseRecorder: httptest.NewRecorder(), ctx: ctx, limit: 16}
	if _, err := w.Write([]byte("x")); err != context.Canceled {
		t.Fatalf("write error=%v", err)
	}
	if _, err := w.WriteString("x"); err != context.Canceled {
		t.Fatalf("string error=%v", err)
	}
	if w.Body.Len() != 0 {
		t.Fatal("cancelled write buffered data")
	}
}

func TestMCPResponseLimitMustBePositive(t *testing.T) {
	for _, n := range []int64{-1, 0} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid limit accepted")
				}
			}()
			WithMCPResponseLimit(n)
		})
	}
}
