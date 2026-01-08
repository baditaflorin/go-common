package response

type Response struct {
	Status string      `json:"status"` // success, error
	Data   interface{} `json:"data,omitempty"`
	Error  *Error      `json:"error,omitempty"`
	Meta   interface{} `json:"meta,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func Success(data interface{}) Response {
	return Response{Status: "success", Data: data}
}

func ErrorResp(code int, msg string) Response {
	return Response{
		Status: "error",
		Error:  &Error{Code: code, Message: msg},
	}
}
