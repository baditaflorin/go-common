package deploymentauthorization

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/deploymentintent"
)

func testIntent(now time.Time) deploymentintent.V1 {
	return deploymentintent.V1{
		SchemaVersion: deploymentintent.SchemaVersion,
		IntentID:      "af441920-b43e-4ab5-a6b8-d0be9468b9a4", ServiceID: "domain-scope-api",
		Environment: "staging", ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		SourceCommit: strings.Repeat("b", 40), TargetPool: "staging-pool",
		Rollout:        deploymentintent.Rollout{Strategy: "rolling", Replicas: 1, MaxUnavailable: 0, MaxSurge: 1, TimeoutSeconds: 300},
		RollbackDigest: "sha256:" + strings.Repeat("c", 64), RequestedBy: "spiffe://staging.0exec.com/ns/fleet/sa/fleet-runner",
		CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(5 * time.Minute),
	}
}

func testStatement(t *testing.T, intent deploymentintent.V1, now time.Time) Statement {
	t.Helper()
	digest, err := deploymentintent.DigestV1(intent)
	if err != nil {
		t.Fatal(err)
	}
	return Statement{
		SchemaVersion: SchemaVersion, SigningKeyID: "authority-staging-1", DecisionID: "90c4b01b-cc2d-4cc6-8d53-b57a0e76d6e4",
		Decision: "allow", Audience: Audience, Principal: "spiffe://staging.0exec.com/ns/fleet/sa/fleet-runner",
		IntentID: intent.IntentID, IntentDigest: digest, ServiceID: intent.ServiceID, Environment: intent.Environment,
		ArtifactDigest: intent.ArtifactDigest, SourceCommit: intent.SourceCommit, TargetPool: intent.TargetPool,
		RollbackDigest: intent.RollbackDigest, CIProvider: "woodpecker", CIRunID: "pipeline/73", CIConclusion: "success",
		CISourceCommit: intent.SourceCommit, ProvenanceIssuer: "ci.0exec.com", ProvenanceBuilder: "fleet-builder",
		ProvenanceArtifact: intent.ArtifactDigest, ProvenanceSourceCommit: intent.SourceCommit,
		PolicyVersion: "phase3/staging-v1", IssuedAt: now, ExpiresAt: now.Add(4 * time.Minute),
	}
}

func TestSignAndVerifyV1RoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	intent := testIntent(now)
	principal := "spiffe://staging.0exec.com/ns/fleet/sa/fleet-runner"
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	statement := testStatement(t, intent, now)
	evidence, err := SignV1(intent, principal, statement, private, now)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyV1(intent, principal, evidence, map[string]ed25519.PublicKey{statement.SigningKeyID: public}, now)
	if err != nil {
		t.Fatal(err)
	}
	if verified.DecisionID != statement.DecisionID || verified.KeyID != statement.SigningKeyID || verified.IntentDigest != statement.IntentDigest || !verified.ExpiresAt.Equal(statement.ExpiresAt) {
		t.Fatalf("verified binding mismatch: %#v", verified)
	}
}

func TestVerifyV1RejectsWrongPrincipalAndIntent(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	intent := testIntent(now)
	principal := "spiffe://staging.0exec.com/ns/fleet/sa/fleet-runner"
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	statement := testStatement(t, intent, now)
	evidence, err := SignV1(intent, principal, statement, private, now)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{statement.SigningKeyID: public}
	if _, err := VerifyV1(intent, principal+"-other", evidence, keys, now); err == nil {
		t.Fatal("wrong principal must be rejected")
	}
	intent.ArtifactDigest = "sha256:" + strings.Repeat("d", 64)
	if _, err := VerifyV1(intent, principal, evidence, keys, now); err == nil {
		t.Fatal("changed artifact digest must be rejected")
	}
}

func TestSignV1RejectsDenyAndInvalidTime(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	intent := testIntent(now)
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	_ = public
	principal := "spiffe://staging.0exec.com/ns/fleet/sa/fleet-runner"
	statement := testStatement(t, intent, now)
	statement.Decision = "deny"
	if _, err := SignV1(intent, principal, statement, private, now); err != ErrDenied {
		t.Fatalf("deny decision error = %v", err)
	}
	statement.Decision = "allow"
	statement.ExpiresAt = now.Add(MaxLifetime + time.Second)
	if _, err := SignV1(intent, principal, statement, private, now); err == nil {
		t.Fatal("overlong decision must be rejected")
	}
}

func TestVerifyV1StrictlyDecodesEnvelopeAndStatement(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	intent := testIntent(now)
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	principal := "spiffe://staging.0exec.com/ns/fleet/sa/fleet-runner"
	statement := testStatement(t, intent, now)
	evidence, err := SignV1(intent, principal, statement, private, now)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{statement.SigningKeyID: public}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(evidence, &envelope); err != nil {
		t.Fatal(err)
	}
	withUnknown, _ := json.Marshal(map[string]json.RawMessage{"payloadType": envelope["payloadType"], "payload": envelope["payload"], "signatures": envelope["signatures"], "unexpected": json.RawMessage(`true`)})
	if _, err := VerifyV1(intent, principal, withUnknown, keys, now); err == nil {
		t.Fatal("unknown envelope fields must be rejected")
	}
	duplicate := []byte(strings.Replace(string(evidence), `"payloadType":`, `"payloadType":"duplicate","payloadType":`, 1))
	if _, err := VerifyV1(intent, principal, duplicate, keys, now); err == nil {
		t.Fatal("duplicate envelope keys must be rejected")
	}
}
