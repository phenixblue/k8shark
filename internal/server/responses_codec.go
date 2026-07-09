package server

import (
	"bytes"
	"mime"
	"net/http"
	"strconv"
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

// isNonProtobufPath reports request paths that never return a Kubernetes
// protobuf object and may be large or streamed — OpenAPI documents (multi-MB
// JSON) and pod log subresources (text/plain). The protobuf response wrapper
// skips these so they aren't buffered in memory only to pass through unchanged.
func isNonProtobufPath(path string) bool {
	return strings.HasPrefix(path, "/openapi") || strings.HasSuffix(path, "/log")
}

// wantsProtobuf reports whether the client's Accept header selects Kubernetes
// protobuf over JSON. It mirrors apiserver negotiation: among acceptable (q>0)
// media types, the highest q wins, and header order breaks ties. So
// `protobuf,json` (both q=1) picks protobuf, `json,protobuf` picks JSON, a
// higher-q entry always wins, and `…protobuf;q=0` / Table requests yield JSON.
func wantsProtobuf(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	// Walk clauses in header order, keeping the first with the maximum q among
	// the types we can serve (protobuf, or JSON/wildcard). "First with max q"
	// makes header order the tie-breaker for equal q-values.
	bestQ := 0.0
	bestIsProto := false
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		var isProto, isJSON bool
		switch mt {
		case protobufMediaType:
			isProto = true
		case "application/json", "application/*", "*/*":
			isJSON = true
		}
		if !isProto && !isJSON {
			continue
		}
		q := 1.0
		if qs, ok := params["q"]; ok {
			if v, perr := strconv.ParseFloat(qs, 64); perr == nil {
				q = v
			}
		}
		if q <= 0 {
			continue
		}
		if q > bestQ { // strictly greater; equal q keeps the earlier clause
			bestQ = q
			bestIsProto = isProto
		}
	}
	return bestIsProto
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

	// The chosen representation depends on Accept, so caches/intermediaries must
	// not reuse it across differing Accept headers.
	p.Header().Add("Vary", "Accept")

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
