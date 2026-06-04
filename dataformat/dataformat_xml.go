package dataformat

import (
	"bytes"
	"encoding/xml"
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
	// Element/attribute names are not text-escaped by encoding/xml; we keep
	// them verbatim (decode produced them from valid XML names).
	buf.WriteString(s)
}
