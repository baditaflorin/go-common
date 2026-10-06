package deploymentintent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// DigestV1 fingerprints the typed intent with stable field ordering and UTC
// timestamps. Call ValidateV1 before using a digest to make an authorization
// decision.
func DigestV1(intent V1) (string, error) {
	intent.CreatedAt = intent.CreatedAt.UTC()
	intent.ExpiresAt = intent.ExpiresAt.UTC()
	canonical, err := json.Marshal(intent)
	if err != nil {
		return "", fmt.Errorf("encode canonical deployment intent: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
