package store

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"

	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/client-go/kubernetes/scheme"
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

const ProtobufMediaType = "application/vnd.kubernetes.protobuf"

// scheme.Scheme registers all built-in Kubernetes types; both serializers are
// stateless over it and safe for concurrent use. internal/server's
// writes_codec.go needs the same pair for the opposite direction (decoding a
// protobuf write body to JSON) and declares its own instances rather than
// importing this package for two lines of construction.
var (
	protobufSerializer = protobuf.NewSerializer(scheme.Scheme, scheme.Scheme)
	jsonSerializer     = kjson.NewSerializerWithOptions(
		kjson.DefaultMetaFactory, scheme.Scheme, scheme.Scheme, kjson.SerializerOptions{})
)

// IsNonProtobufPath reports request paths that never return a Kubernetes
// protobuf object and may be large or streamed — OpenAPI documents (multi-MB
// JSON) and pod log subresources (text/plain). The protobuf response wrapper
// skips these so they aren't buffered in memory only to pass through unchanged.
func IsNonProtobufPath(path string) bool {
	return strings.HasPrefix(path, "/openapi") || strings.HasSuffix(path, "/log")
}

// WantsProtobuf reports whether the client's Accept header selects Kubernetes
// protobuf over JSON. It mirrors apiserver negotiation: among acceptable (q>0)
// media types, the highest q wins, and header order breaks ties. So
// `protobuf,json` (both q=1) picks protobuf, `json,protobuf` picks JSON, a
// higher-q entry always wins, and `…protobuf;q=0` / Table requests yield JSON.
func WantsProtobuf(r *http.Request) bool {
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
		case ProtobufMediaType:
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

// ProtobufResponseWriter buffers a response so a JSON body of a built-in type
// can be re-encoded as protobuf on flush. It is only used for non-watch requests
// whose client prefers protobuf.
type ProtobufResponseWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func NewProtobufResponseWriter(w http.ResponseWriter) *ProtobufResponseWriter {
	return &ProtobufResponseWriter{ResponseWriter: w}
}

func (p *ProtobufResponseWriter) WriteHeader(code int) { p.status = code }

func (p *ProtobufResponseWriter) Write(b []byte) (int, error) { return p.buf.Write(b) }

// Flush writes the buffered response, transcoding a JSON built-in-object body to
// protobuf when possible; otherwise it passes the JSON through unchanged.
func (p *ProtobufResponseWriter) Flush() {
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
			p.Header().Set("Content-Type", ProtobufMediaType)
			p.Header().Del("Content-Length") // length changed; let net/http recompute
			p.ResponseWriter.WriteHeader(status)
			_, _ = p.ResponseWriter.Write(pb)
			return
		}
	}
	p.ResponseWriter.WriteHeader(status)
	_, _ = p.ResponseWriter.Write(body)
}

// ── PartialObjectMetadata projection (issue #329) ────────────────────────────
//
// kube-controller-manager's garbagecollector walks ownerReferences using a
// metadata-only client, requesting `as=PartialObjectMetadata`. Ignoring that
// parameter and returning the full object doesn't merely waste bytes — the
// client decodes strictly against the negotiated type and fails outright:
//
//	unable to decode returned object as PartialObjectMetadata:
//	invalid character 'k' looking for beginning of value
//
// which left the GC unable to sync any item and retrying forever. Projecting the
// body down to apiVersion/kind/metadata is what a real apiserver does for this
// Accept parameter, and it's cheap: the metadata is already in the response.

const (
	partialMetadataKind     = "PartialObjectMetadata"
	partialMetadataListKind = "PartialObjectMetadataList"
	metaGroup               = "meta.k8s.io"
)

// WantsPartialObjectMetadata reports whether the client's Accept header asks for
// a metadata-only projection, and returns the meta.k8s.io version to answer in
// (v1 or v1beta1). Only `g=meta.k8s.io` counts: `as=Table` uses the same
// parameter style and is handled separately.
//
// q-values are honored the same way WantsProtobuf honors them — highest q wins,
// header order breaks ties, q=0 means "not acceptable". Returning on the first
// syntactic match instead would project even when the client explicitly
// de-prioritized or disabled that clause, e.g.
// `application/json, application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1;q=0`.
func WantsPartialObjectMetadata(r *http.Request) (string, bool) {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return "", false
	}
	bestQ := 0.0
	bestVersion := ""
	bestIsPartial := false
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		// Only clauses in a representation we can actually serve compete; an
		// unrelated media type shouldn't outrank a metadata clause.
		switch mt {
		case ProtobufMediaType, "application/json", "application/*", "*/*":
		default:
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
		isPartial := params["as"] == partialMetadataKind && params["g"] == metaGroup
		if q > bestQ { // strictly greater; equal q keeps the earlier clause
			bestQ = q
			bestIsPartial = isPartial
			if isPartial {
				if bestVersion = params["v"]; bestVersion == "" {
					bestVersion = "v1"
				}
			} else {
				bestVersion = ""
			}
		}
	}
	if !bestIsPartial {
		return "", false
	}
	return bestVersion, true
}

// PartialMetadataResponseWriter buffers a response so a JSON object or list body
// can be projected to PartialObjectMetadata(List) on flush.
//
// Anything that isn't a recognizable object or list — a Status from a 404, an
// OpenAPI document, plain text — passes through untouched. That matters: error
// responses must stay decodable as Status, or a client would see a decode
// failure instead of the NotFound it was told to expect.
type PartialMetadataResponseWriter struct {
	http.ResponseWriter
	metaVersion string
	status      int
	buf         bytes.Buffer
}

func NewPartialMetadataResponseWriter(w http.ResponseWriter, metaVersion string) *PartialMetadataResponseWriter {
	return &PartialMetadataResponseWriter{ResponseWriter: w, metaVersion: metaVersion}
}

func (p *PartialMetadataResponseWriter) WriteHeader(code int) { p.status = code }

func (p *PartialMetadataResponseWriter) Write(b []byte) (int, error) { return p.buf.Write(b) }

// Flush writes the buffered response, projecting a JSON body to metadata-only
// when it is a Kubernetes object or list; otherwise it passes through unchanged.
func (p *PartialMetadataResponseWriter) Flush() {
	status := p.status
	if status == 0 {
		status = http.StatusOK
	}
	body := p.buf.Bytes()

	// The representation depends on Accept, so it must not be cached across
	// differing Accept headers.
	p.Header().Add("Vary", "Accept")

	if strings.HasPrefix(p.Header().Get("Content-Type"), "application/json") {
		if projected, ok := projectPartialMetadata(body, p.metaVersion); ok {
			p.Header().Del("Content-Length") // length changed
			p.ResponseWriter.WriteHeader(status)
			_, _ = p.ResponseWriter.Write(projected)
			return
		}
	}
	p.ResponseWriter.WriteHeader(status)
	_, _ = p.ResponseWriter.Write(body)
}

// projectPartialMetadata rewrites a Kubernetes object or list JSON body as
// PartialObjectMetadata or PartialObjectMetadataList. It reports false for
// anything it doesn't recognize — a Status, a non-object, malformed JSON — so
// the caller passes those through.
func projectPartialMetadata(body []byte, metaVersion string) ([]byte, bool) {
	var probe struct {
		Kind     string            `json:"kind"`
		Metadata json.RawMessage   `json:"metadata"`
		Items    []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, false
	}
	// A Status is an error/result envelope, not an object to project. Leaving it
	// alone keeps 404s decodable as Status.
	if probe.Kind == "" || probe.Kind == "Status" {
		return nil, false
	}
	apiVersion := metaGroup + "/" + metaVersion

	// List: project each item, preserving the list's own metadata
	// (resourceVersion/continue) so watch bookmarks and paging still line up.
	if strings.HasSuffix(probe.Kind, "List") {
		out := map[string]any{
			"kind":       partialMetadataListKind,
			"apiVersion": apiVersion,
		}
		if len(probe.Metadata) > 0 {
			out["metadata"] = probe.Metadata
		}
		items := make([]map[string]any, 0, len(probe.Items))
		for _, raw := range probe.Items {
			var it struct {
				Metadata json.RawMessage `json:"metadata"`
			}
			if err := json.Unmarshal(raw, &it); err != nil || len(it.Metadata) == 0 {
				// Refuse the whole projection rather than emit a shorter list.
				// Dropping an item would change the response's meaning, and the
				// caller most likely to ask for this projection is the garbage
				// collector — for which a silently missing item can read as "the
				// owner is gone". Passing the original list through is the
				// fail-safe: the client sees real data it can't project, not
				// projected data that lies about what exists.
				return nil, false
			}
			items = append(items, map[string]any{
				"kind":       partialMetadataKind,
				"apiVersion": apiVersion,
				"metadata":   it.Metadata,
			})
		}
		out["items"] = items
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil, false
		}
		return encoded, true
	}

	// Single object.
	if len(probe.Metadata) == 0 {
		return nil, false
	}
	encoded, err := json.Marshal(map[string]any{
		"kind":       partialMetadataKind,
		"apiVersion": apiVersion,
		"metadata":   probe.Metadata,
	})
	if err != nil {
		return nil, false
	}
	return encoded, true
}
