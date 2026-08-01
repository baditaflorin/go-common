package dataformat

import (
	"bytes"
	"encoding/xml"
	"strings"
	"unicode"
)

// xmlNode mirrors an arbitrary XML element for generic decoding.
type xmlNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Children []xmlNode  `xml:",any"`
	Content  string     `xml:",chardata"`
}

func xmlEscapeText(buf *bytes.Buffer, s string) {
	_ = xml.EscapeText(buf, []byte(s))
}

func xmlEscapeName(buf *bytes.Buffer, s string) {
	// Element/attribute names are not text-escaped by encoding/xml; on the
	// decode side that is safe because xml.Unmarshal only ever hands us names
	// it parsed out of well-formed markup. On the ENCODE side the name comes
	// from an arbitrary decoded key (a JSON/YAML/TOML/CSV field name), which
	// carries no such guarantee. Every caller here MUST have already checked
	// isValidXMLName(s) — see writeXMLElement / splitXMLFields callers in
	// dataformat_write.go — otherwise a key containing e.g. `</a><b>` would
	// close the enclosing tag and inject new markup instead of being rendered
	// as inert text.
	buf.WriteString(s)
}

// isValidXMLName reports whether s can be written verbatim as an XML 1.0
// element or attribute name. It is a deliberately conservative subset of the
// XML Name production (ASCII-oriented start/continue rules, plus the
// reserved "xml*" prefix) — good enough to guarantee well-formed markup for
// every real-world field name, while rejecting anything (spaces, `<`, `>`,
// `&`, quotes, empty string, a leading digit, ...) that would otherwise let
// an arbitrary decoded key produce malformed or injected XML.
func isValidXMLName(s string) bool {
	if s == "" {
		return false
	}
	if len(s) >= 3 && strings.EqualFold(s[:3], "xml") {
		// Reserved by the XML 1.0 spec regardless of case (xml, xmlns, ...).
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !isXMLNameStartChar(r) {
				return false
			}
			continue
		}
		if !isXMLNameChar(r) {
			return false
		}
	}
	return true
}

func isXMLNameStartChar(r rune) bool {
	return r == '_' || r == ':' || unicode.IsLetter(r)
}

func isXMLNameChar(r rune) bool {
	return isXMLNameStartChar(r) || r == '-' || r == '.' || unicode.IsDigit(r)
}
