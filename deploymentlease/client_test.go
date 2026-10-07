package deploymentlease

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/deploymentauthorization"
	"github.com/baditaflorin/go-common/deploymentintent"
)

const testPrincipal = "spiffe://staging.0exec.com/ns/fleet/sa/fleet-runner"

func TestClientAcquiresVerifiesAndManagesLease(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	intent := testIntent(now)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	statement := testStatement(intent, now)
	envelope, err := deploymentauthorization.SignV1(intent, testPrincipal, statement, privateKey, now)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := "af441920-b43e-4ab5-a6b8-d0be9468b9a5"
	lease := Lease{
		LeaseID: "af441920-b43e-4ab5-a6b8-d0be9468b9a6", DecisionID: statement.DecisionID,
		AttemptID: attemptID, IntentID: intent.IntentID,
		IntentDigest: mustIntentDigest(t, intent), ArtifactDigest: intent.ArtifactDigest,
		SourceCommit: intent.SourceCommit, RollbackDigest: intent.RollbackDigest, Principal: testPrincipal,
		ServiceID: intent.ServiceID, Environment: intent.Environment, TargetPool: intent.TargetPool,
		TargetID: "staging-runner-01", PolicyVersion: statement.PolicyVersion, Fence: 8, State: "active",
		IssuedAt: now, ExpiresAt: now.Add(90 * time.Second), IntentExpiresAt: intent.ExpiresAt,
	}
	var actions []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request method/content type: %s %q", r.Method, r.Header.Get("Content-Type"))
		}
		switch r.URL.Path {
		case "/v1/authorizations":
			actions = append(actions, "authorize")
			w.Header().Set("Content-Type", "application/vnd.dsse.envelope.v1+json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(envelope)
		case "/v1/leases":
			actions = append(actions, "acquire")
			var body struct {
				AttemptID string          `json:"attempt_id"`
				TargetID  string          `json:"target_id"`
				Intent    json.RawMessage `json:"intent"`
				Auth      json.RawMessage `json:"authorization"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode acquire body: %v", err)
			}
			if body.AttemptID != attemptID || body.TargetID != lease.TargetID || string(body.Auth) != string(envelope) {
				t.Errorf("unexpected acquire binding: %+v", body)
			}
			var gotIntent deploymentintent.V1
			if err := json.Unmarshal(body.Intent, &gotIntent); err != nil || gotIntent.IntentID != intent.IntentID {
				t.Errorf("unexpected acquire intent: err=%v intent=%+v", err, gotIntent)
			}
			writeTestJSON(w, http.StatusOK, lease)
		case "/v1/leases/" + lease.LeaseID + "/validate":
			actions = append(actions, "validate")
			writeTestJSON(w, http.StatusOK, lease)
		case "/v1/leases/" + lease.LeaseID + "/renew":
			actions = append(actions, "renew")
			lease.ExpiresAt = time.Now().UTC().Add(2 * time.Minute)
			writeTestJSON(w, http.StatusOK, lease)
		case "/v1/leases/" + lease.LeaseID + "/finish":
			actions = append(actions, "finish")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newWithHTTP(testConfig(server.URL, publicKey), server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Acquire(context.Background(), intent, attemptID, lease.TargetID)
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	if got.LeaseID != lease.LeaseID || got.Fence != lease.Fence {
		t.Fatalf("unexpected lease: %+v", got)
	}
	if _, err := client.Validate(context.Background(), got); err != nil {
		t.Fatalf("validate lease: %v", err)
	}
	renewed, err := client.Renew(context.Background(), got, 2*time.Minute)
	if err != nil {
		t.Fatalf("renew lease: %v", err)
	}
	if err := client.Finish(context.Background(), renewed, "completed"); err != nil {
		t.Fatalf("finish lease: %v", err)
	}
	wantActions := []string{"authorize", "acquire", "validate", "renew", "finish"}
	if strings.Join(actions, ",") != strings.Join(wantActions, ",") {
		t.Fatalf("request sequence = %v, want %v", actions, wantActions)
	}
}

func TestClientRejectsInvalidAuthorityResponseAndDecision(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	intent := testIntent(now)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	statement := testStatement(intent, now)
	envelope, err := deploymentauthorization.SignV1(intent, testPrincipal, statement, privateKey, now)
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{
		LeaseID: "af441920-b43e-4ab5-a6b8-d0be9468b9a6", DecisionID: statement.DecisionID,
		AttemptID: "af441920-b43e-4ab5-a6b8-d0be9468b9a5", IntentID: intent.IntentID,
		IntentDigest: mustIntentDigest(t, intent), ArtifactDigest: intent.ArtifactDigest,
		SourceCommit: intent.SourceCommit, RollbackDigest: intent.RollbackDigest, Principal: testPrincipal,
		ServiceID: intent.ServiceID, Environment: intent.Environment, TargetPool: intent.TargetPool,
		TargetID: "unexpected-target", PolicyVersion: statement.PolicyVersion, Fence: 8, State: "active",
		IssuedAt: now, ExpiresAt: now.Add(90 * time.Second), IntentExpiresAt: intent.ExpiresAt,
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/authorizations" {
			w.Header().Set("Content-Type", "application/vnd.dsse.envelope.v1+json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(envelope)
			return
		}
		writeTestJSON(w, http.StatusOK, lease)
	}))
	defer server.Close()
	client, err := newWithHTTP(testConfig(server.URL, publicKey), server.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Acquire(context.Background(), intent, "af441920-b43e-4ab5-a6b8-d0be9468b9a5", "staging-runner-01")
	if err != ErrInvalidLease {
		t.Fatalf("Acquire error = %v, want invalid lease", err)
	}

	badEnvelope := []byte(strings.Replace(string(envelope), deploymentauthorization.PayloadType, "text/plain", 1))
	badServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.dsse.envelope.v1+json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(badEnvelope)
	}))
	defer badServer.Close()
	badClient, err := newWithHTTP(testConfig(badServer.URL, publicKey), badServer.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = badClient.Acquire(context.Background(), intent, "af441920-b43e-4ab5-a6b8-d0be9468b9a5", "staging-runner-01")
	if err != ErrRejected {
		t.Fatalf("Acquire with bad evidence error = %v, want rejected", err)
	}
}

func TestNewWithHTTPRejectsInsecureOrAmbiguousConfiguration(t *testing.T) {
	publicKey := make(ed25519.PublicKey, ed25519.PublicKeySize)
	for _, endpoint := range []string{"http://authority.local", "https://user:pass@authority.local", "https://authority.local?debug=1", "https://authority.local/#fragment"} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := testConfig(endpoint, publicKey)
			if _, err := newWithHTTP(cfg, http.DefaultClient, nil); err != ErrInvalidConfig {
				t.Fatalf("NewWithHTTP error = %v, want invalid config", err)
			}
		})
	}
}

func testConfig(endpoint string, key ed25519.PublicKey) Config {
	return Config{Endpoint: endpoint, Principal: testPrincipal, SigningKeys: map[string]ed25519.PublicKey{"authority-staging-1": key}}
}

func testIntent(now time.Time) deploymentintent.V1 {
	return deploymentintent.V1{
		SchemaVersion: deploymentintent.SchemaVersion, IntentID: "af441920-b43e-4ab5-a6b8-d0be9468b9a4",
		ServiceID: "domain-scope-api", Environment: "staging", ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		SourceCommit: strings.Repeat("b", 40), TargetPool: "staging-pool",
		Rollout:        deploymentintent.Rollout{Strategy: "rolling", Replicas: 1, MaxUnavailable: 0, MaxSurge: 1, TimeoutSeconds: 300},
		RollbackDigest: "sha256:" + strings.Repeat("c", 64), RequestedBy: testPrincipal,
		CreatedAt: now.Add(-20 * time.Second), ExpiresAt: now.Add(4 * time.Minute),
	}
}

func testStatement(intent deploymentintent.V1, now time.Time) deploymentauthorization.Statement {
	digest, _ := deploymentintent.DigestV1(intent)
	return deploymentauthorization.Statement{
		SchemaVersion: deploymentauthorization.SchemaVersion, SigningKeyID: "authority-staging-1",
		DecisionID: "af441920-b43e-4ab5-a6b8-d0be9468b9a7", Decision: "allow",
		Audience: deploymentauthorization.Audience, Principal: testPrincipal,
		IntentID: intent.IntentID, IntentDigest: digest, ServiceID: intent.ServiceID,
		Environment: intent.Environment, ArtifactDigest: intent.ArtifactDigest, SourceCommit: intent.SourceCommit,
		TargetPool: intent.TargetPool, RollbackDigest: intent.RollbackDigest,
		CIProvider: "woodpecker", CIRunID: "pipeline/73", CIConclusion: "success", CISourceCommit: intent.SourceCommit,
		ProvenanceIssuer: "ci.0exec.com", ProvenanceBuilder: "https://github.com/baditaflorin/go_fleet_runner#publish-image",
		ProvenanceArtifact: intent.ArtifactDigest, ProvenanceSourceCommit: intent.SourceCommit,
		PolicyVersion: "phase3/staging-v1", IssuedAt: now, ExpiresAt: now.Add(3 * time.Minute),
	}
}

func mustIntentDigest(t *testing.T, intent deploymentintent.V1) string {
	t.Helper()
	digest, err := deploymentintent.DigestV1(intent)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func writeTestJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
