package dataformat

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
)

// encodeCSV requires a tabular value: an []any of objects (keys become the
// header, unioned and sorted) or an []any of []any (raw rows). Anything else
// returns ErrNotTabular.
func encodeCSV(v any) ([]byte, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: CSV requires an array at the top level", ErrNotTabular)
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	if len(arr) == 0 {
		w.Flush()
		return buf.Bytes(), w.Error()
	}

	// Raw rows: array of arrays.
	if _, isArr := arr[0].([]any); isArr {
		for _, row := range arr {
			cells, ok := row.([]any)
			if !ok {
				return nil, fmt.Errorf("%w: mixed row types in array-of-arrays", ErrNotTabular)
			}
			rec := make([]string, len(cells))
			for i, c := range cells {
				rec[i] = scalarToString(c)
			}
			if err := w.Write(rec); err != nil {
				return nil, err
			}
		}
		w.Flush()
		return buf.Bytes(), w.Error()
	}

	// Array of objects: union the keys for a stable header.
	keySet := map[string]struct{}{}
	objs := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		m, ok := asStringMap(item)
		if !ok {
			return nil, fmt.Errorf("%w: array elements must be objects", ErrNotTabular)
		}
		for k := range m {
			keySet[k] = struct{}{}
		}
		objs = append(objs, m)
	}
	header := make([]string, 0, len(keySet))
	for k := range keySet {
		header = append(header, k)
	}
	sort.Strings(header)

	if err := w.Write(header); err != nil {
		return nil, err
	}
	for _, m := range objs {
		rec := make([]string, len(header))
		for i, h := range header {
			if val, present := m[h]; present {
				rec[i] = scalarToString(val)
			}
		}
		if err := w.Write(rec); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

// encodeXML renders a map[string]any with exactly one root key. Other shapes
// (multiple roots, top-level array or scalar) return ErrUnsupportedShape
// because XML has no canonical rendering for them.
func encodeXML(v any) ([]byte, error) {
	m, ok := asStringMap(v)
	if !ok {
		return nil, fmt.Errorf("%w: XML requires a single-rooted object", ErrUnsupportedShape)
	}
	if len(m) != 1 {
		return nil, fmt.Errorf("%w: XML requires exactly one root element, got %d", ErrUnsupportedShape, len(m))
	}
	var root string
	var body any
	for k, val := range m {
		root, body = k, val
	}
	var buf bytes.Buffer
	if err := writeXMLElement(&buf, root, body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
