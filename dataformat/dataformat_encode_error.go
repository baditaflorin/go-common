package dataformat

import (
	"fmt"
)

// EncodeError wraps an underlying encode failure with the format.
type EncodeError struct {
	Format Format
	Err    error
}

func (e *EncodeError) Error() string {
	return fmt.Sprintf("dataformat: encode %s: %v", e.Format, e.Err)
}

func (e *EncodeError) Unwrap() error { return e.Err }
