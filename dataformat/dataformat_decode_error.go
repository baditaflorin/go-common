package dataformat

import (
	"fmt"
)

// DecodeError wraps an underlying decode/parse failure with the format.
type DecodeError struct {
	Format Format
	Err    error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("dataformat: decode %s: %v", e.Format, e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }
