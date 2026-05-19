// Package depcheck provides a registry of named health probes whose
// results are included in the /health endpoint JSON.
//
// Services register probes (e.g. ping a database, check a file) via
// Registry.Register. The server calls Snapshot on every /health request
// and surfaces {status:"degraded"} if any probe fails (HTTP stays 200).
package depcheck
