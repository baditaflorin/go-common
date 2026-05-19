// Package response provides the canonical fleet HTTP response envelope.
//
// Every fleet JSON response wraps its payload in a consistent shape:
//
//	{"status":"success", "data":{...}, "_emitted_at":"...", "_schema_version":N}
//	{"status":"error",   "error":{"code":403,"error_code":"auth.forbidden","message":"..."}}
//
// Use Success(data), NewError(status, code, msg), or ErrorResp(status, msg).
// The Envelope function merges fleet metadata fields into any JSON payload.
package response
