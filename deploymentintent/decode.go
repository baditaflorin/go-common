package deploymentintent

import (
	"fmt"
	"time"

	"github.com/baditaflorin/go-common/internal/strictjson"
)

// DecodeV1 parses one bounded JSON intent, rejecting duplicate keys and
// unknown fields before applying the schema's time-dependent validation.
func DecodeV1(body []byte, now time.Time) (V1, error) {
	if len(body) == 0 || len(body) > MaxBodyBytes {
		return V1{}, fmt.Errorf("input must be between 1 byte and %d bytes", MaxBodyBytes)
	}
	var intent V1
	if err := strictjson.Decode(body, &intent); err != nil {
		return V1{}, fmt.Errorf("decode schema v1: %w", err)
	}
	if err := ValidateV1(intent, now); err != nil {
		return V1{}, err
	}
	return intent, nil
}
