package dataformat

import "testing"

var fuzzFormats = []Format{JSON, CSV, XML, YAML, TOML}

// FuzzDecode feeds arbitrary bytes to every decoder. Hostile input may be
// rejected, but it must never panic, and a value that did decode must
// re-encode into every other format (or return an error) without panicking.
func FuzzDecode(f *testing.F) {
	f.Add(uint8(0), []byte(`{"a":1,"b":[1,2,{"c":null}]}`))
	f.Add(uint8(1), []byte("id,name\n1,alice\n2,\"b,ob\"\n"))
	f.Add(uint8(2), []byte(`<root><a x="1">t</a><a>u</a></root>`))
	f.Add(uint8(3), []byte("a: &x [1, 2]\nb: *x\nc: {d: e}\n"))
	f.Add(uint8(4), []byte("title = \"t\"\n[owner]\nname = \"n\"\n"))
	f.Fuzz(func(t *testing.T, sel uint8, data []byte) {
		from := fuzzFormats[int(sel)%len(fuzzFormats)]
		v, err := Decode(from, data)
		if err != nil {
			return
		}
		for _, to := range fuzzFormats {
			_, _ = Encode(to, v)
		}
	})
}

// FuzzDetectFormat checks the sniffer never panics and that a detected format
// can be handed to Decode without a panic (Decode may still reject the input).
func FuzzDetectFormat(f *testing.F) {
	f.Add([]byte(`{"a":1}`))
	f.Add([]byte("a,b\n1,2\n"))
	f.Add([]byte("<a/>"))
	f.Add([]byte("k: v\n"))
	f.Add([]byte("k = 1\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		fm, err := DetectFormat(data)
		if err != nil {
			return
		}
		_, _ = Decode(fm, data)
	})
}

// FuzzConvert runs the whole decode -> encode pipeline for every format pair.
func FuzzConvert(f *testing.F) {
	f.Add(uint8(0), uint8(1), []byte(`[{"a":1},{"a":2}]`))
	f.Add(uint8(3), uint8(0), []byte("a: 1\nb: [x, y]\n"))
	f.Fuzz(func(t *testing.T, a, b uint8, data []byte) {
		from := fuzzFormats[int(a)%len(fuzzFormats)]
		to := fuzzFormats[int(b)%len(fuzzFormats)]
		_, _ = Convert(from, to, data)
	})
}
