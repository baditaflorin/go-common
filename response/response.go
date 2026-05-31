package response

import "fmt"

// Response is the canonical fleet API response envelope.
// All fleet HTTP handlers should return their data through this shape.
type Response struct {
	Status string      `json:"status"` // "success" or "error"
	Data   interface{} `json:"data,omitempty"`
	Error  *Error      `json:"error,omitempty"`
	Meta   interface{} `json:"meta,omitempty"`
}

// Error is the error sub-object in a Response. It carries both an
// HTTP status code and a stable machine-readable error_code so clients
// can switch on Code without parsing the human-readable Message.
//
// JSON shape:
//
//	{
//	  "status": "error",
//	  "error": {
//	    "code": 403,
//	    "error_code": "auth.scope_mismatch",
//	    "message": "token scope does not cover this resource"
//	  }
//	}
type Error struct {
	// Code is the HTTP status code (403, 404, 500, …).
	Code int `json:"code"`
	// ErrorCode is a stable, dotted, machine-readable identifier,
	// e.g. "auth.scope_mismatch", "not_found", "bad_request.validation".
	// Clients should switch on this field, not on Message.
	ErrorCode string `json:"error_code,omitempty"`
	// Message is a human-readable description safe to surface in responses.
	Message string `json:"message"`
}

// Error implements the error interface so a decoded error envelope
// (see DecodeData) can be returned and compared as a normal error.
// The message is server-provided and safe to surface; it never
// carries secret payload data.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.ErrorCode != "" {
		return fmt.Sprintf("%s (http %d): %s", e.ErrorCode, e.Code, e.Message)
	}
	return fmt.Sprintf("http %d: %s", e.Code, e.Message)
}

// Success wraps data in a successful Response envelope.
func Success(data interface{}) Response {
	return Response{Status: "success", Data: data}
}

// ErrorResp creates an error Response with only an HTTP status code and
// message. Prefer NewError when you have a stable error_code.
func ErrorResp(code int, msg string) Response {
	return Response{
		Status: "error",
		Error:  &Error{Code: code, Message: msg},
	}
}

// NewError creates an error Response with a machine-readable error_code.
// This is the canonical constructor for all fleet API error responses.
//
//	response.NewError(http.StatusForbidden, "auth.scope_mismatch",
//	    "token scope does not cover this resource")
func NewError(httpStatus int, errorCode, message string) Response {
	return Response{
		Status: "error",
		Error: &Error{
			Code:      httpStatus,
			ErrorCode: errorCode,
			Message:   message,
		},
	}
}

// FromError creates an error Response from a standard error value.
// If err is nil, Success(nil) is returned. If err carries an *errors.Error
// (from go-common/errors) the HTTP status and error_code are used directly.
// Otherwise a synthetic 500 internal error is returned.
//
// Note: importing go-common/errors here would create an import cycle since
// errors/ is a leaf package. Callers that use go-common/errors should use
// NewError directly.
func FromError(err error) Response {
	if err == nil {
		return Success(nil)
	}
	return NewError(500, "internal", err.Error())
}
