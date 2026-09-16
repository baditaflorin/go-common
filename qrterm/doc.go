// Package qrterm turns a short piece of content — a URL, an id, a token —
// into a QR code an operator can scan straight off a terminal, or an
// image/png a browser/client can render. It exists because the same
// "?qr=" pattern was hand-rolled independently in go-fleet-pipe and was
// about to be re-copied into half a dozen more fleet services; this is
// the one place that logic lives now.
//
// The core idea: push from a headless box, then scan the code straight
// off your terminal with your phone — no typing a URL, no retyping an
// id. Half-block terminal art (via QueryFormat's default "text" format)
// scans well from most terminal emulators and is copy-paste safe; PNG
// is for browsers and anything that wants an actual image.
//
// Typical wiring in an HTTP handler:
//
//	if f := qrterm.QueryFormat(r); f != "" {
//	    if err := qrterm.Write(w, f, content); err != nil {
//	        httpError(w, http.StatusInternalServerError, "qr encode failed")
//	        return
//	    }
//	    return
//	}
//
// Usage examples (curl):
//
//	cmd | curl --data-binary @- 'https://pipe.0exec.com/?qr'    # push + print a
//	                                                              # scannable QR
//	curl 'https://pipe.0exec.com/<id>?qr'                        # QR for an id
//	curl 'https://pipe.0exec.com/<id>?qr=png' > link.png         # PNG instead
//
// qrterm can only build a QR for content the server itself can see
// (plain ids, plain URLs). For end-to-end-encrypted content where the
// key lives in a URL #fragment the server never receives, the QR must
// be drawn client-side instead — qrterm has nothing to encode in that
// case.
package qrterm
