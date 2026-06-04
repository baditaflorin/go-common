package dataformat

import (
	"bytes"
	"encoding/csv"
	"io"
	"strings"
)

func looksLikeTOML(b []byte) bool {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// [table] / [[array-of-tables]]
		if strings.HasPrefix(line, "[") {
			return true
		}
		// key = value (TOML mandates the spaces-optional '=' assignment)
		if eq := strings.IndexByte(line, '='); eq > 0 {
			// Distinguish from YAML "key: value" by requiring no ':' before '='.
			if !strings.ContainsRune(line[:eq], ':') {
				return true
			}
		}
		return false
	}
	return false
}

func looksLikeCSV(b []byte) bool {
	first := b
	if nl := bytes.IndexByte(b, '\n'); nl >= 0 {
		first = b[:nl]
	}
	if !bytes.ContainsRune(first, ',') {
		return false
	}
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = 0 // enforce consistent column count after first row
	cols := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false
		}
		if cols == 0 {
			cols = len(rec)
		}
	}
	return cols >= 2
}
