package dataformat

import (
	"bytes"
	"encoding/csv"
	"encoding/xml"
)

// decodeCSV reads CSV with the first row as a header and returns
// []any of map[string]any rows. Every cell is a string.
func decodeCSV(b []byte) (any, error) {
	r := csv.NewReader(bytes.NewReader(b))
	records, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return []any{}, nil
	}
	header := records[0]
	rows := make([]any, 0, len(records)-1)
	for _, rec := range records[1:] {
		row := make(map[string]any, len(header))
		for i, h := range header {
			if i < len(rec) {
				row[h] = rec[i]
			} else {
				row[h] = ""
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// decodeXML parses a single-rooted XML document into a map[string]any keyed
// by the root element name. Attributes are folded in under "-name" keys and
// text content under "#text"; repeated children collapse into an []any.
func decodeXML(b []byte) (any, error) {
	var root xmlNode
	if err := xml.Unmarshal(b, &root); err != nil {
		return nil, err
	}
	return map[string]any{root.XMLName.Local: nodeToValue(root)}, nil
}
