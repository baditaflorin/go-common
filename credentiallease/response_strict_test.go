package credentiallease

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseResponseRejectsAmbiguousJSONAndCleansSafeID(t *testing.T) {
	expiry := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name, fields string
		wantRevoke   bool
	}{
		{"credential duplicate", `"lease_id":"lease_safe","credential":"first","credential":"second"`, true},
		{"lease id duplicate", `"lease_id":"lease_safe","lease_id":"lease_other","credential":"value"`, false},
		{"same id repeated", `"lease_id":"lease_safe","lease_id":"lease_safe","credential":"value"`, false},
		{"unknown field", `"lease_id":"lease_safe","credential":"value","extra":true`, true},
		{"nested duplicate", `"lease_id":"lease_safe","credential":"value","extra":{"x":1,"x":2}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var revoked, called atomic.Bool
			broker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if r.URL.Path != "/v1/leases/lease_safe" {
						t.Errorf("cleanup targeted an ambiguous lease: %s", r.URL.Path)
					}
					revoked.Store(true)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{%s,"expires_at":%q}`, tc.fields, expiry)
			}))
			defer broker.Close()
			client := testClient(t, broker, IdentityTokenSourceFunc(func(context.Context, string) (string, error) { return "proof", nil }), nil)
			err := client.WithLease(context.Background(), testRequest(), func(context.Context, *Lease) error {
				called.Store(true)
				return nil
			})
			if !errors.Is(err, ErrInvalidLease) || called.Load() || revoked.Load() != tc.wantRevoke {
				t.Fatalf("error=%v callback=%v revoked=%v; want invalid lease, no callback, revoked=%v", err, called.Load(), revoked.Load(), tc.wantRevoke)
			}
		})
	}
}

func TestTaskResponseRejectsAmbiguousJSONAndCleansSafeIdentity(t *testing.T) {
	expiry := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name, fields string
		wantRevoke   bool
	}{
		{"duplicate expiry", `"task_id":"task_safe","proof":"header.payload.signature","expires_at":"2000-01-01T00:00:00Z"`, true},
		{"duplicate task id", `"task_id":"task_safe","task_id":"task_other","proof":"header.payload.signature"`, false},
		{"duplicate proof", `"task_id":"task_safe","proof":"first","proof":"header.payload.signature"`, false},
		{"unknown field", `"task_id":"task_safe","proof":"header.payload.signature","extra":1`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var revoked atomic.Bool
			broker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if r.URL.Path != "/v1/tasks/task_safe" || r.Header.Get("Authorization") != "Bearer "+taskTestProof {
						t.Error("cleanup used ambiguous identity")
					}
					revoked.Store(true)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{%s,"expires_at":%q}`, tc.fields, expiry)
			}))
			defer broker.Close()
			manager := newTaskTestManager(t, broker)
			task, err := manager.BeginTask(context.Background())
			if !errors.Is(err, ErrTaskCreateFailed) || task != nil || revoked.Load() != tc.wantRevoke {
				t.Fatalf("error=%v task=%v revoked=%v; want rejection and revoked=%v", err, task, revoked.Load(), tc.wantRevoke)
			}
		})
	}
}

func TestResponseCleanupStrings(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{`{"task_id":"safe","proof":"proof"} {"task_id":"other"}`, []string{"safe", "proof"}},
		{`{"task_id":"safe","TASK_ID":"alias","proof":"proof"}`, []string{"", "proof"}},
		{`{"task_id":"safe","task_id":"second","task_id":"third","proof":"proof"}`, []string{"", "proof"}},
		{`{"task_id":"safe","proof":"proof",`, []string{"", ""}},
		{`[]`, []string{"", ""}},
		{`{"task_id":17,"proof":"proof"}`, []string{"", "proof"}},
	} {
		if got := responseCleanupStrings([]byte(tc.raw), "task_id", "proof"); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("cleanup fields = %q, want %q", got, tc.want)
		}
	}
}
