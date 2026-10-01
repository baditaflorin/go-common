// Package broker provides the fail-closed HTTP core for credentiallease v1.
//
// Applications should normally import credentiallease as clients. A broker
// deployment wires this package to a trust-domain proof verifier, exact-scope
// policy, durable encrypted lease store, protected audit sink, and narrowly
// scoped provider issuers. No production verifier, store, audit backend, or
// provider credential is embedded here; in-memory fakes belong in tests only.
package broker
