// Package dataformat provides bidirectional conversion primitives between
// the structured data formats commonly seen across the fleet: JSON, CSV,
// XML, YAML and TOML.
//
// The package exposes a small, format-agnostic surface:
//
//	DetectFormat(b []byte) (Format, error)   // best-effort sniff
//	Decode(f Format, b []byte) (any, error)  // bytes  -> Go value
//	Encode(f Format, v any) ([]byte, error)  // Go value -> bytes
//	Convert(from, to Format, b []byte) ([]byte, error)
//
// Decode always yields a "generic" Go value built from map[string]any,
// []any and scalars (string/float64/bool/nil), so a value decoded from one
// format can be re-encoded into any other without a concrete struct.
//
// # Lossy edges
//
// Not every format pair is total. The conversions are intentionally
// explicit about where information is lost or where a shape is required:
//
//   - CSV is tabular. Encoding to CSV requires either an array of objects
//     with consistent (string-keyed) fields, or an array of arrays. Decoding
//     CSV infers column names from the header row and produces an
//     []any of map[string]any rows; every cell value is a string (CSV has
//     no type system). Converting a non-tabular value to CSV returns
//     ErrNotTabular.
//
//   - XML has no native arrays or a typed scalar model, and it distinguishes
//     attributes from child elements. Decode unmarshals XML into a
//     map[string]any tree where attributes are folded in with a "-" prefix
//     and character data is stored under "#text"; repeated sibling elements
//     become an []any. Round-tripping arbitrary JSON/YAML/TOML through XML is
//     therefore lossy: there is no canonical way to render an array or a
//     top-level scalar as XML. Encoding a value XML cannot faithfully
//     represent returns ErrUnsupportedShape.
//
//   - TOML requires a table (map) at the top level; it cannot encode a bare
//     array or scalar as a document. Encoding such a value returns
//     ErrUnsupportedShape.
//
// All shape/parse failures are reported through typed errors (see the
// Err* sentinels and the *Error types) so callers can branch with
// errors.Is / errors.As.
package dataformat
