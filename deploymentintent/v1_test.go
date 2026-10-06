package deploymentintent

import (
	"strings"
	"testing"
	"time"
)

func validIntent(now time.Time) V1 {
	return V1{
		SchemaVersion: SchemaVersion, IntentID: "7d0c1c42-88ae-4b70-8c71-ff941f934f06",
		ServiceID: "example-service", Environment: "staging",
		ArtifactDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceCommit:   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TargetPool:     "staging", Rollout: Rollout{Strategy: "rolling", Replicas: 1, MaxUnavailable: 0, MaxSurge: 1, TimeoutSeconds: 900, HealthySeconds: 60},
		RollbackDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		RequestedBy:    "agent-session:task-123", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
}

func TestDecodeV1AcceptsValidIntentAndRejectsAmbiguousJSON(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	valid := `{"schema_version":"v1","intent_id":"7d0c1c42-88ae-4b70-8c71-ff941f934f06","service_id":"example-service","environment":"staging","artifact_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","source_commit":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","target_pool":"staging","rollout":{"strategy":"rolling","replicas":1,"max_unavailable":0,"max_surge":1,"timeout_seconds":900,"healthy_seconds":60},"rollback_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","requested_by":"agent-session:task-123","created_at":"2026-10-07T12:00:00Z","expires_at":"2026-10-07T12:15:00Z"}`
	decoded, err := DecodeV1([]byte(valid), now)
	if err != nil {
		t.Fatalf("decode valid intent: %v", err)
	}
	if decoded.ServiceID != "example-service" || decoded.Rollout.Replicas != 1 {
		t.Fatalf("decoded intent is incomplete: %#v", decoded)
	}
	for name, body := range map[string]string{
		"duplicate top-level": strings.Replace(valid, `"environment":"staging"`, `"environment":"staging","environment":"production"`, 1),
		"duplicate nested":    strings.Replace(valid, `"replicas":1`, `"replicas":1,"replicas":2`, 1),
		"unknown field":       strings.Replace(valid, `"environment":"staging"`, `"environment":"staging","host":"10.0.0.1"`, 1),
		"trailing object":     valid + `{}`,
		"null object":         `null`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeV1([]byte(body), now); err == nil {
				t.Fatal("invalid intent accepted")
			}
		})
	}
}

func TestDigestV1NormalizesTimeZonesAndBindsChanges(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	first := validIntent(now)
	firstDigest, err := DigestV1(first)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.CreatedAt = first.CreatedAt.In(time.FixedZone("UTC+2", 2*60*60))
	second.ExpiresAt = first.ExpiresAt.In(time.FixedZone("UTC+2", 2*60*60))
	secondDigest, err := DigestV1(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("equivalent times changed digest: %q != %q", firstDigest, secondDigest)
	}
	second.TargetPool = "prod"
	changedDigest, err := DigestV1(second)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == firstDigest {
		t.Fatal("deployment-critical field did not affect digest")
	}
}

func TestValidateV1EnforcesEnvironmentAndBoundedRollout(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	base := validIntent(now)
	if err := ValidateV1(base, now); err != nil {
		t.Fatalf("valid intent rejected: %v", err)
	}
	invalid := []struct {
		name   string
		mutate func(*V1)
	}{
		{"production requires approval", func(i *V1) { i.Environment = "production" }},
		{"hostname is not a pool", func(i *V1) { i.TargetPool = "10.0.0.1" }},
		{"tag is not a digest", func(i *V1) { i.ArtifactDigest = "latest" }},
		{"oversized replica count", func(i *V1) { i.Rollout.Replicas = 513 }},
		{"invalid strategy", func(i *V1) { i.Rollout.Strategy = "replace" }},
		{"expired", func(i *V1) { i.ExpiresAt = now }},
		{"overlong lifetime", func(i *V1) { i.ExpiresAt = now.Add(MaxLifetime + time.Second) }},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			intent := base
			test.mutate(&intent)
			if err := ValidateV1(intent, now); err == nil {
				t.Fatal("invalid intent accepted")
			}
		})
	}
}

func TestDecodeV1RejectsBodyOutsideSizeLimit(t *testing.T) {
	if _, err := DecodeV1(make([]byte, MaxBodyBytes+1), time.Now()); err == nil {
		t.Fatal("oversized intent accepted")
	}
}
