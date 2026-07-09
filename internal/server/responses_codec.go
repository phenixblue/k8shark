package server

import (
	"bytes"
	"net/http"
	"strings"
)

// Response content negotiation for a uniform apiserver surface (issue #150).
//
// client-go/kubectl default to protobuf (application/vnd.kubernetes.protobuf)
// for built-in types. The mock builds every response as JSON; a protobuf client
// still decodes those (negotiation is by the *response* Content-Type), so reads
// already work — but a real apiserver replies with protobuf when the client asks
// for it. To match, we buffer a non-watch response and, if its JSON body is a
// scheme-known object, re-encode it as protobuf. Bodies that aren't built-in
// Kubernetes objects (CRDs/unstructured, OpenAPI docs, health text) fail the
// scheme decode and pass through as JSON — exactly as a real apiserver does
// (CRDs have no protobuf).

const protobufMediaType = "application/vnd.kubernetes.protobuf"

// wantsProtobuf reports whether the request's Accept header prefers Kubernetes
// protobuf. Table requests (kubectl's human output) never include it.
func wantsProtobuf(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), protobufMediaType)
}

// jsonToProtobuf re-encodes a JSON-encoded built-in Kubernetes object as
// protobuf. ok is false when the body isn't a scheme-known object, so the caller
// leaves it as JSON.
func jsonToProtobuf(body []byte) ([]byte, bool) {
	obj, _, err := jsonSerializer.Decode(body, nil, nil)
	if err != nil {
		return nil, false
	}
	var buf bytes.Buffer
	if err := protobufSerializer.Encode(obj, &buf); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// protobufResponseWriter buffers a response so a JSON body of a built-in type
// can be re-encoded as protobuf on flush. It is only used for non-watch requests
// whose client prefers protobuf.
type protobufResponseWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func newProtobufResponseWriter(w http.ResponseWriter) *protobufResponseWriter {
	return &protobufResponseWriter{ResponseWriter: w}
}

func (p *protobufResponseWriter) WriteHeader(code int) { p.status = code }

func (p *protobufResponseWriter) Write(b []byte) (int, error) { return p.buf.Write(b) }

// flush writes the buffered response, transcoding a JSON built-in-object body to
// protobuf when possible; otherwise it passes the JSON through unchanged.
func (p *protobufResponseWriter) flush() {
	status := p.status
	if status == 0 {
		status = http.StatusOK
	}
	body := p.buf.Bytes()

	ct := p.Header().Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		if pb, ok := jsonToProtobuf(body); ok {
			p.Header().Set("Content-Type", protobufMediaType)
			p.Header().Del("Content-Length") // length changed; let net/http recompute
			p.ResponseWriter.WriteHeader(status)
			_, _ = p.ResponseWriter.Write(pb)
			return
		}
	}
	p.ResponseWriter.WriteHeader(status)
	_, _ = p.ResponseWriter.Write(body)
}
