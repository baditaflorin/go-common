// Package backoffcoord provides a distributed backoff coordinator client.
// Before retrying a failed upstream call, services consult the coordinator
// to avoid thundering-herd pile-ons when the upstream recovers.
package backoffcoord
