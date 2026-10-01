// Package credentiallease is a task-bound client for a credential broker.
//
// The package does not issue provider credentials or hold broker administrator
// credentials. A workload presents a short-lived identity proof to a separate
// broker, asks for one credential bound to a task, audience, resource, actions,
// and delivery method, uses it only inside a callback, and asks the broker to
// revoke the lease when the callback returns. The broker's TTL remains the
// cleanup backstop if the client exits or revocation cannot complete.
//
// Credential bytes are kept private to Lease and can only be used by its Do
// method to send an HTTPS request to the target origin fixed at acquisition.
// Do disables redirects. Applications must not log authenticated request
// headers or expose request details to model-visible tool output.
//
// This package defines the client side of the v1 broker protocol. A broker
// implementation must independently authenticate workload proofs, enforce
// task/audience/resource/action policy, audit issuance and revocation, and
// issue provider credentials with a bounded TTL.
//
// The optional Observer receives secret-free acquire, target-use, and revoke
// outcomes. Its use event includes only result, HTTP status, and duration.
package credentiallease
