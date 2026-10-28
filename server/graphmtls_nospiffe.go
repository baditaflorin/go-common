//go:build gocommon_nospiffe

package server

import (
	"context"
	"errors"
	"time"

	"github.com/baditaflorin/go-common/graph"
)

// errNoSPIFFE is what graph's sender logs (on its first attempt and every tenth after it)
// when GRAPH_SPIFFE_COLLECTOR_URL is set in a binary built with -tags gocommon_nospiffe. Without the variable the factory
// is never called, so a service that does not use the SPIFFE path sees no difference.
var errNoSPIFFE = errors.New("server: GRAPH_SPIFFE_COLLECTOR_URL needs the SPIFFE Workload API client, " +
	"which this binary was built without (-tags gocommon_nospiffe): rebuild without the tag or unset the variable")

func graphMTLSClient(context.Context, string, []string, time.Duration) (graph.MTLSClient, error) {
	return nil, errNoSPIFFE
}
