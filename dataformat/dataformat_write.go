package dataformat

import (
	"bytes"
	"fmt"
)

func writeXMLElement(buf *bytes.Buffer, name string, v any) error {
	switch val := v.(type) {
	case map[string]any:
		attrs, children, text := splitXMLFields(val)
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
