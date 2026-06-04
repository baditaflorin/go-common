package dataformat

import (
	"fmt"
)

// Format identifies a structured data serialization.
type Format int

// String returns the canonical lower-case name of the format.
func (f Format) String() string {
	switch f {
	case JSON:
		return "json"
	case CSV:
		return "csv"
	case XML:
		return "xml"
	case YAML:
		return "yaml"
	case TOML:
		return "toml"
	default:
		return fmt.Sprintf("Format(%d)", int(f))
	}
}
