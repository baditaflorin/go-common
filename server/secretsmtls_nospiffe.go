//go:build gocommon_nospiffe

package server

import (
	"context"
	"errors"
	"time"

	"github.com/baditaflorin/go-common/secrets"
)

// errNoSPIFFESecrets is what the secrets client logs when FLEET_SECRETS_SPIFFE=dual is set in a binary built with
// -tags gocommon_nospiffe; reads then use the API key. Without the variable the factory is never called.
var errNoSPIFFESecrets = errors.New("server: FLEET_SECRETS_SPIFFE needs the SPIFFE Workload API client, " +
	"which this binary was built without (-tags gocommon_nospiffe): rebuild without the tag or unset the variable")

func secretsSPIFFEClient(context.Context, string, []string, time.Duration) (secrets.SPIFFEClient, error) {
	return nil, errNoSPIFFESecrets
}
