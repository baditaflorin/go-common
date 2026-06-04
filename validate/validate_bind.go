package validate

import (
	"encoding/json"
	fleetErrors "github.com/baditaflorin/go-common/errors"
	"net/http"
)

// Bind decodes the JSON body of r into dst (which must be a non-nil
// pointer to a struct), then validates all `validate` struct tags.
// Returns a *fleetErrors.Error with Status=400 on any decode or
// validation failure. Returns nil on success.
func Bind(r *http.Request, dst any) *fleetErrors.Error {
	if err := decodeJSON(r, dst); err != nil {
		return fleetErrors.New(http.StatusBadRequest, "bad_request.json", err.Error())
	}
	return Struct(dst)
}

// BindBytes decodes b (raw JSON) into dst, then validates.
func BindBytes(b []byte, dst any) *fleetErrors.Error {
	if err := json.Unmarshal(b, dst); err != nil {
		return fleetErrors.New(http.StatusBadRequest, "bad_request.json",
			"invalid JSON: "+err.Error())
	}
	return Struct(dst)
}
