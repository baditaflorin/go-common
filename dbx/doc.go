// Package dbx is the common, context-aware database entry point for Go Common.
//
// OpenPostgres provides instrumented database/sql access through pgx. OpenPGXPool
// provides a native pgxpool with a trace-safe query tracer. OpenSQL supports
// other registered database/sql drivers. OpenSQL caps pools at 10 open / 5 idle
// connections by default, with bounded connection lifetime and initial ping.
// Spans record operation type and outcome, and omit SQL statements, bind
// values, connection strings, and raw driver error messages.
//
// BuildSelect, BuildInsert, BuildUpdate, and BuildDelete generate PostgreSQL
// CRUD statements with bound values and validated/quoted identifiers. CopyFrom
// provides typed pgx binary COPY with the same identifier validation. Resolve
// external names through a fixed application allowlist before calling Ident;
// syntactic validation alone is not an authorization policy. For complex
// queries, prefer generated sqlc methods and context-aware database/sql APIs.
// Never concatenate caller-controlled values into SQL. Identifiers cannot be
// represented as bind parameters and must be selected from trusted code.
package dbx
