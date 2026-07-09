package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/client-go/kubernetes/scheme"
)

// Write bodies arrive as JSON or, from client-go/kubectl/controllers by default,
// as Kubernetes protobuf (application/vnd.kubernetes.protobuf). The overlay
// stores objects as JSON, so protobuf bodies are decoded to a typed object via
// the built-in scheme and re-encoded to JSON at the write ingress; everything
// downstream stays JSON. (See issue #148.)
var (
	// scheme.Scheme registers all built-in Kubernetes types; both serializers
	// are stateless over it and safe for concurrent use.
	protobufSerializer = protobuf.NewSerializer(scheme.Scheme, scheme.Scheme)
	jsonSerializer     = kjson.NewSerializerWithOptions(
		kjson.DefaultMetaFactory, scheme.Scheme, scheme.Scheme, kjson.SerializerOptions{})
)

// isProtobufContentType reports whether ct is the Kubernetes protobuf media type
// (ignoring any parameters, e.g. a charset).
func isProtobufContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/vnd.kubernetes.protobuf")
}

// protobufToJSON decodes a Kubernetes protobuf (k8s\x00-framed) body into a
// typed object using the built-in scheme, then re-encodes it as JSON. It only
// handles types registered in the scheme (i.e. built-in Kubernetes kinds);
// CRDs/unstructured are sent by clients as JSON, not protobuf.
func protobufToJSON(raw []byte) (json.RawMessage, error) {
	obj, _, err := protobufSerializer.Decode(raw, nil, nil)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jsonSerializer.Encode(obj, &buf); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

// readObjectBody reads a create/replace request body and returns it as JSON,
// transparently decoding a Kubernetes protobuf body first. On any error it
// writes the appropriate Status response and returns ok=false.
func (h *handler) readObjectBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBytes))
	if err != nil {
		h.writeStatus(w, http.StatusBadRequest, "reading body: "+err.Error())
		return nil, false
	}
	if isProtobufContentType(r.Header.Get("Content-Type")) {
		body, err := protobufToJSON(raw)
		if err != nil {
			h.writeStatus(w, http.StatusBadRequest, "decoding protobuf body: "+err.Error())
			return nil, false
		}
		return body, true
	}
	// JSON (or unspecified content type): require a JSON object, as before.
	if !isJSONObject(raw) {
		h.writeStatus(w, http.StatusBadRequest, "request body must be a JSON object")
		return nil, false
	}
	return raw, true
}
