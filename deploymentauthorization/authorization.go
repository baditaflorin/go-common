// Package deploymentauthorization defines the signed deployment-decision
// contract shared by the authority and fleet runner.
package deploymentauthorization

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/baditaflorin/go-common/deploymentintent"
	"github.com/secure-systems-lab/go-securesystemslib/dsse"
)

const (
	SchemaVersion    = "v1"
	Audience         = "go_fleet_runner"
	PayloadType      = "application/vnd.domainscope.deployment-authorization.v1+json"
	MaxEnvelopeBytes = 64 << 10
	MaxPayloadBytes  = 32 << 10
	MaxLifetime      = 10 * time.Minute
)

var (
	keyIDPattern            = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	policyPattern           = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	runIDPattern            = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,127}$`)
	evidenceIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/#-]{0,254}$`)
	ErrDenied               = errors.New("deployment authorization denied")
)

// Statement is the exact v1 authority decision payload consumed by
// go_fleet_runner. It contains evidence summaries only, never raw provider
// responses, credentials, hostnames, or command text.
type Statement struct {
	SchemaVersion          string     `json:"schema_version"`
	SigningKeyID           string     `json:"signing_key_id"`
	DecisionID             string     `json:"decision_id"`
	Decision               string     `json:"decision"`
	Audience               string     `json:"audience"`
	Principal              string     `json:"principal"`
	IntentID               string     `json:"intent_id"`
	IntentDigest           string     `json:"intent_digest"`
	ServiceID              string     `json:"service_id"`
	Environment            string     `json:"environment"`
	ArtifactDigest         string     `json:"artifact_digest"`
	SourceCommit           string     `json:"source_commit"`
	TargetPool             string     `json:"target_pool"`
	RollbackDigest         string     `json:"rollback_digest"`
	ApprovalRef            string     `json:"approval_ref,omitempty"`
	ApprovalActor          string     `json:"approval_actor,omitempty"`
	ApprovalVerifiedAt     *time.Time `json:"approval_verified_at,omitempty"`
	CIProvider             string     `json:"ci_provider"`
	CIRunID                string     `json:"ci_run_id"`
	CIConclusion           string     `json:"ci_conclusion"`
	CISourceCommit         string     `json:"ci_source_commit"`
	ProvenanceIssuer       string     `json:"provenance_issuer"`
	ProvenanceBuilder      string     `json:"provenance_builder"`
	ProvenanceArtifact     string     `json:"provenance_artifact_digest"`
	ProvenanceSourceCommit string     `json:"provenance_source_commit"`
	PolicyVersion          string     `json:"policy_version"`
	IssuedAt               time.Time  `json:"issued_at"`
	ExpiresAt              time.Time  `json:"expires_at"`
}

// Verified is returned only for a trusted, correctly bound allow statement.
type Verified struct {
	DecisionID    string
	KeyID         string
	Principal     string
	IntentDigest  string
	PolicyVersion string
	ExpiresAt     time.Time
}

// SignV1 validates and signs a complete allow statement with one Ed25519 key.
// The key ID appears in both the DSSE envelope and payload so it cannot be
// confused with an untrusted envelope hint.
func SignV1(intent deploymentintent.V1, principal string, statement Statement, key ed25519.PrivateKey, now time.Time) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("deployment authorization signing key is invalid")
	}
	if err := deploymentintent.ValidateV1(intent, now); err != nil {
		return nil, fmt.Errorf("invalid deployment intent: %w", err)
	}
	if err := validateStatement(intent, principal, statement, now); err != nil {
		return nil, err
	}
	if statement.Decision != "allow" {
		return nil, ErrDenied
	}
	payload, err := json.Marshal(statement)
	if err != nil || len(payload) > MaxPayloadBytes {
		return nil, errors.New("deployment authorization payload is invalid")
	}
	signer, err := dsse.NewEnvelopeSigner(ed25519Signer{keyID: statement.SigningKeyID, key: key})
	if err != nil {
		return nil, errors.New("deployment authorization signer configuration is invalid")
	}
	envelope, err := signer.SignPayload(context.Background(), PayloadType, payload)
	if err != nil {
		return nil, errors.New("deployment authorization signing failed")
	}
	encoded, err := json.Marshal(envelope)
	if err != nil || len(encoded) > MaxEnvelopeBytes {
		return nil, errors.New("deployment authorization envelope is invalid")
	}
	return encoded, nil
}

// VerifyV1 verifies a signed allow decision against the already authenticated
// principal and validated typed intent. Trust keys must come from protected
// configuration; keys in the evidence are never accepted as trust roots.
func VerifyV1(intent deploymentintent.V1, principal string, evidence []byte, trustedKeys map[string]ed25519.PublicKey, now time.Time) (Verified, error) {
	if err := deploymentintent.ValidateV1(intent, now); err != nil {
		return Verified{}, fmt.Errorf("invalid deployment intent: %w", err)
	}
	if !deploymentintent.ValidActor(principal) {
		return Verified{}, errors.New("authenticated principal is required")
	}
	if len(evidence) == 0 || len(evidence) > MaxEnvelopeBytes {
		return Verified{}, errors.New("authorization evidence size is invalid")
	}
	if len(trustedKeys) == 0 {
		return Verified{}, errors.New("authorization trust keys are unavailable")
	}
	var envelope dsse.Envelope
	if err := decodeStrict(evidence, &envelope); err != nil {
		return Verified{}, fmt.Errorf("decode authorization envelope: %w", err)
	}
	if envelope.PayloadType != PayloadType || len(envelope.Signatures) != 1 {
		return Verified{}, errors.New("unsupported authorization envelope")
	}
	signature := envelope.Signatures[0]
	if !keyIDPattern.MatchString(signature.KeyID) {
		return Verified{}, errors.New("authorization key id is invalid")
	}
	publicKey, ok := trustedKeys[signature.KeyID]
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return Verified{}, errors.New("authorization signing key is not trusted")
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil || len(payload) == 0 || len(payload) > MaxPayloadBytes || base64.StdEncoding.EncodeToString(payload) != envelope.Payload {
		return Verified{}, errors.New("authorization payload encoding is invalid")
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(signature.Sig)
	if err != nil || len(signatureBytes) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(signatureBytes) != signature.Sig {
		return Verified{}, errors.New("authorization signature encoding is invalid")
	}
	verifier, err := dsse.NewEnvelopeVerifier(ed25519Verifier{keyID: signature.KeyID, key: publicKey})
	if err != nil {
		return Verified{}, errors.New("authorization verifier configuration is invalid")
	}
	accepted, decoded, err := verifier.VerifyAndDecode(context.Background(), &envelope)
	if err != nil || len(accepted) != 1 || accepted[0].KeyID != signature.KeyID || !bytes.Equal(decoded, payload) {
		return Verified{}, errors.New("authorization signature verification failed")
	}
	var statement Statement
	if err := decodeStrict(payload, &statement); err != nil {
		return Verified{}, fmt.Errorf("decode signed authorization statement: %w", err)
	}
	if statement.SigningKeyID != signature.KeyID {
		return Verified{}, errors.New("authorization statement key id does not match envelope")
	}
	if err := validateStatement(intent, principal, statement, now); err != nil {
		return Verified{}, err
	}
	if statement.Decision != "allow" {
		return Verified{}, ErrDenied
	}
	intentDigest, err := deploymentintent.DigestV1(intent)
	if err != nil {
		return Verified{}, fmt.Errorf("fingerprint deployment intent: %w", err)
	}
	return Verified{DecisionID: statement.DecisionID, KeyID: signature.KeyID, Principal: principal, IntentDigest: intentDigest, PolicyVersion: statement.PolicyVersion, ExpiresAt: statement.ExpiresAt.UTC()}, nil
}

func validateStatement(intent deploymentintent.V1, principal string, statement Statement, now time.Time) error {
	if statement.SchemaVersion != SchemaVersion {
		return errors.New("unsupported authorization statement version")
	}
	if !keyIDPattern.MatchString(statement.SigningKeyID) {
		return errors.New("authorization statement signing key id is invalid")
	}
	if !deploymentintent.ValidUUID(statement.DecisionID) {
		return errors.New("authorization decision id is invalid")
	}
	if statement.Decision != "allow" && statement.Decision != "deny" {
		return errors.New("authorization decision must be allow or deny")
	}
	if statement.Audience != Audience || statement.Principal != principal {
		return errors.New("authorization audience or principal does not match")
	}
	intentDigest, err := deploymentintent.DigestV1(intent)
	if err != nil {
		return fmt.Errorf("fingerprint deployment intent: %w", err)
	}
	if statement.IntentID != intent.IntentID || statement.IntentDigest != intentDigest {
		return errors.New("authorization does not bind to this intent")
	}
	if statement.ServiceID != intent.ServiceID || statement.Environment != intent.Environment || statement.ArtifactDigest != intent.ArtifactDigest ||
		statement.SourceCommit != intent.SourceCommit || statement.TargetPool != intent.TargetPool || statement.RollbackDigest != intent.RollbackDigest {
		return errors.New("authorization deployment fields do not match this intent")
	}
	if statement.ApprovalRef != intent.ApprovalRef {
		return errors.New("authorization approval reference does not match this intent")
	}
	if !deploymentintent.ValidActor(statement.CIProvider) || !runIDPattern.MatchString(statement.CIRunID) || statement.CIConclusion != "success" || statement.CISourceCommit != intent.SourceCommit {
		return errors.New("authorization CI evidence is incomplete or mismatched")
	}
	if !evidenceIdentityPattern.MatchString(statement.ProvenanceIssuer) || !evidenceIdentityPattern.MatchString(statement.ProvenanceBuilder) ||
		statement.ProvenanceArtifact != intent.ArtifactDigest || statement.ProvenanceSourceCommit != intent.SourceCommit {
		return errors.New("authorization artifact provenance is incomplete or mismatched")
	}
	if !policyPattern.MatchString(statement.PolicyVersion) {
		return errors.New("authorization policy version is invalid")
	}
	if intent.Environment == "production" || intent.ApprovalRef != "" {
		if !deploymentintent.ValidActor(statement.ApprovalActor) || statement.ApprovalVerifiedAt == nil {
			return errors.New("authorization requires verified approval evidence")
		}
		if statement.ApprovalVerifiedAt.After(statement.IssuedAt) || statement.ApprovalVerifiedAt.After(now) {
			return errors.New("production approval evidence timestamp is invalid")
		}
	} else if statement.ApprovalActor != "" || statement.ApprovalVerifiedAt != nil {
		if !deploymentintent.ValidActor(statement.ApprovalActor) || statement.ApprovalVerifiedAt == nil ||
			statement.ApprovalVerifiedAt.After(statement.IssuedAt) || statement.ApprovalVerifiedAt.After(now) {
			return errors.New("authorization approval evidence timestamp is invalid")
		}
	}
	if statement.IssuedAt.IsZero() || statement.ExpiresAt.IsZero() || statement.IssuedAt.Before(intent.CreatedAt) || statement.IssuedAt.After(now.Add(30*time.Second)) ||
		!statement.ExpiresAt.After(now) || !statement.ExpiresAt.After(statement.IssuedAt) || statement.ExpiresAt.After(intent.ExpiresAt) || statement.ExpiresAt.Sub(statement.IssuedAt) > MaxLifetime {
		return errors.New("authorization statement validity window is invalid")
	}
	return nil
}

type ed25519Signer struct {
	keyID string
	key   ed25519.PrivateKey
}

func (s ed25519Signer) Sign(_ context.Context, message []byte) ([]byte, error) {
	return ed25519.Sign(s.key, message), nil
}
func (s ed25519Signer) KeyID() (string, error) { return s.keyID, nil }

type ed25519Verifier struct {
	keyID string
	key   ed25519.PublicKey
}

func (v ed25519Verifier) Verify(_ context.Context, message, signature []byte) error {
	if !ed25519.Verify(v.key, message, signature) {
		return errors.New("signature mismatch")
	}
	return nil
}
func (v ed25519Verifier) KeyID() (string, error)   { return v.keyID, nil }
func (v ed25519Verifier) Public() crypto.PublicKey { return v.key }

func decodeStrict(data []byte, destination any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("input must contain exactly one JSON object")
		}
		return fmt.Errorf("trailing input: %w", err)
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder); err != nil {
		return fmt.Errorf("invalid JSON structure: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("input must contain exactly one JSON object")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}
