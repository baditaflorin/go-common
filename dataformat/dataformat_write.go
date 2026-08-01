package dataformat

import (
	"bytes"
	"fmt"
)

func writeXMLElement(buf *bytes.Buffer, name string, v any) error {
	// Reject before writing anything: the name comes from a decoded key that
	// may be arbitrary user/scrape data (e.g. a JSON object field), not
	// something known to be a valid XML name. Emitting it unescaped would
	// either produce malformed markup (a space, a leading digit, ...) or,
	// worse, let a key containing "</foo><bar>" close the enclosing tag and
	// inject sibling markup instead of being rendered as an element name.
	if !isValidXMLName(name) {
		return fmt.Errorf("%w: %q is not a valid XML element name", ErrUnsupportedShape, name)
	}
	switch val := v.(type) {
	case map[string]any:
		attrs, children, text := splitXMLFields(val)
		for _, a := range attrs {
			if !isValidXMLName(a.k) {
				return fmt.Errorf("%w: %q is not a valid XML attribute name", ErrUnsupportedShape, a.k)
			}
		}
		buf.WriteByte('<')
		xmlEscapeName(buf, name)
		for _, a := range attrs {
			buf.WriteByte(' ')
			xmlEscapeName(buf, a.k)
			buf.WriteString(`="`)
			xmlEscapeText(buf, scalarToString(a.v))
			buf.WriteByte('"')
		}
		buf.WriteByte('>')
		if text != "" {
			xmlEscapeText(buf, text)
		}
		for _, c := range children {
			if err := writeXMLChild(buf, c.k, c.v); err != nil {
				return err
			}
		}
		buf.WriteString("</")
		xmlEscapeName(buf, name)
		buf.WriteByte('>')
		return nil
	case []any:
		// Arrays under a named element become repeated elements.
		return fmt.Errorf("%w: cannot render array as the value of <%s>", ErrUnsupportedShape, name)
	default:
		buf.WriteByte('<')
		xmlEscapeName(buf, name)
		buf.WriteByte('>')
		xmlEscapeText(buf, scalarToString(v))
		buf.WriteString("</")
		xmlEscapeName(buf, name)
		buf.WriteByte('>')
		return nil
	}
}

func writeXMLChild(buf *bytes.Buffer, name string, v any) error {
	if arr, ok := v.([]any); ok {
		for _, item := range arr {
			if err := writeXMLElement(buf, name, item); err != nil {
				return err
			}
		}
		return nil
	}
	return writeXMLElement(buf, name, v)
}
