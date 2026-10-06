// Package deploymentintent defines and validates the versioned deployment
// intent exchanged by fleet deployment control-plane components.
package deploymentintent

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	SchemaVersion = "v1"
	MaxBodyBytes  = 1 << 20
	MaxLifetime   = time.Hour
)

var (
	intentUUIDPattern        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	serviceIDPattern         = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	digestPattern            = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitPattern            = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	poolPattern              = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62})$`)
	actorPattern             = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,127}$`)
	approvalReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,127}$`)
)

// V1 is an immutable deployment request. It carries logical identifiers and
// digests only; it must never grow host addresses, credentials, shell commands,
// or arbitrary runtime configuration.
type V1 struct {
	SchemaVersion  string    `json:"schema_version"`
	IntentID       string    `json:"intent_id"`
	ServiceID      string    `json:"service_id"`
	Environment    string    `json:"environment"`
	ArtifactDigest string    `json:"artifact_digest"`
	SourceCommit   string    `json:"source_commit"`
	TargetPool     string    `json:"target_pool"`
	Rollout        Rollout   `json:"rollout"`
	RollbackDigest string    `json:"rollback_digest"`
	RequestedBy    string    `json:"requested_by"`
	ApprovalRef    string    `json:"approval_ref,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type Rollout struct {
	Strategy       string `json:"strategy"`
	Replicas       int    `json:"replicas"`
	MaxUnavailable int    `json:"max_unavailable"`
	MaxSurge       int    `json:"max_surge"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	HealthySeconds int    `json:"healthy_seconds"`
}

func ValidUUID(value string) bool { return intentUUIDPattern.MatchString(value) }
func ValidServiceID(value string) bool {
	return len(value) <= 128 && serviceIDPattern.MatchString(value)
}
func ValidDigest(value string) bool            { return digestPattern.MatchString(value) }
func ValidCommit(value string) bool            { return commitPattern.MatchString(value) }
func ValidPool(value string) bool              { return poolPattern.MatchString(value) }
func ValidActor(value string) bool             { return actorPattern.MatchString(value) }
func ValidApprovalReference(value string) bool { return approvalReferencePattern.MatchString(value) }

func ValidateV1(intent V1, now time.Time) error {
	if intent.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %q", SchemaVersion)
	}
	if !ValidUUID(intent.IntentID) {
		return errors.New("intent_id must be a UUID")
	}
	if !ValidServiceID(intent.ServiceID) {
		return errors.New("service_id must be a lowercase canonical service identifier")
	}
	if intent.Environment != "staging" && intent.Environment != "production" {
		return errors.New("environment must be staging or production")
	}
	if !ValidDigest(intent.ArtifactDigest) || !ValidDigest(intent.RollbackDigest) {
		return errors.New("artifact_digest and rollback_digest must be immutable sha256 digests")
	}
	if !ValidCommit(intent.SourceCommit) {
		return errors.New("source_commit must be a full 40- or 64-character lowercase Git object ID")
	}
	if !ValidPool(intent.TargetPool) {
		return errors.New("target_pool must be a logical pool identifier, not a hostname or address")
	}
	if intent.Rollout.Strategy != "rolling" {
		return errors.New("rollout.strategy must be rolling in schema v1")
	}
	if intent.Rollout.Replicas < 1 || intent.Rollout.Replicas > 512 {
		return errors.New("rollout.replicas must be between 1 and 512")
	}
	if intent.Rollout.MaxUnavailable < 0 || intent.Rollout.MaxUnavailable > intent.Rollout.Replicas {
		return errors.New("rollout.max_unavailable must be between 0 and replicas")
	}
	if intent.Rollout.MaxSurge < 0 || intent.Rollout.MaxSurge > 512 {
		return errors.New("rollout.max_surge must be between 0 and 512")
	}
	if intent.Rollout.MaxSurge == 0 && intent.Rollout.MaxUnavailable >= intent.Rollout.Replicas {
		return errors.New("rollout cannot take every replica unavailable without a surge replica")
	}
	if intent.Rollout.TimeoutSeconds < 30 || intent.Rollout.TimeoutSeconds > 7200 {
		return errors.New("rollout.timeout_seconds must be between 30 and 7200")
	}
	if intent.Rollout.HealthySeconds < 0 || intent.Rollout.HealthySeconds > 3600 {
		return errors.New("rollout.healthy_seconds must be between 0 and 3600")
	}
	if !ValidActor(intent.RequestedBy) {
		return errors.New("requested_by must be a bounded actor or task identifier")
	}
	if intent.Environment == "production" && !ValidApprovalReference(intent.ApprovalRef) {
		return errors.New("production intents require a bounded approval_ref")
	}
	if intent.ApprovalRef != "" && !ValidApprovalReference(intent.ApprovalRef) {
		return errors.New("approval_ref contains unsupported characters")
	}
	if intent.CreatedAt.IsZero() || intent.ExpiresAt.IsZero() {
		return errors.New("created_at and expires_at must be RFC3339 timestamps")
	}
	if intent.CreatedAt.After(now.Add(5 * time.Minute)) {
		return errors.New("created_at is more than five minutes in the future")
	}
	if !intent.ExpiresAt.After(now) || !intent.ExpiresAt.After(intent.CreatedAt) {
		return errors.New("expires_at must be in the future and after created_at")
	}
	if intent.ExpiresAt.Sub(intent.CreatedAt) > MaxLifetime {
		return errors.New("deployment intent lifetime cannot exceed one hour")
	}
	return nil
}
