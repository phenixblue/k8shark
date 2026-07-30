package server

import (
	"encoding/json"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestStatusObj_ReasonMatchesClientGo guards #255: statusObj previously
// omitted "reason", so client-go's apierrors helpers — which key off reason,
// not code — always returned false against the mock server. Round-trip
// statusObj through JSON into a real metav1.Status wrapped in a StatusError,
// the same shape client-go builds from a response body, and assert the
// matching apierrors.Is* helper recognizes it.
func TestStatusObj_ReasonMatchesClientGo(t *testing.T) {
	cases := []struct {
		code  int
		check func(error) bool
	}{
		{http.StatusBadRequest, apierrors.IsBadRequest},
		{http.StatusForbidden, apierrors.IsForbidden},
		{http.StatusMethodNotAllowed, apierrors.IsMethodNotSupported},
		{http.StatusInternalServerError, apierrors.IsInternalError},
		{http.StatusConflict, apierrors.IsConflict},
		{http.StatusUnprocessableEntity, apierrors.IsInvalid},
	}
	for _, c := range cases {
		obj := statusObj(c.code, "boom")
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal statusObj(%d): %v", c.code, err)
		}
		var status metav1.Status
		if err := json.Unmarshal(data, &status); err != nil {
			t.Fatalf("unmarshal into metav1.Status: %v", err)
		}
		statusErr := &apierrors.StatusError{ErrStatus: status}
		if !c.check(statusErr) {
			t.Errorf("code %d: apierrors helper returned false for reason %q — statusObj is missing/wrong reason", c.code, status.Reason)
		}
	}
}

func TestStatusReasonForCode(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:          "BadRequest",
		http.StatusUnauthorized:        "Unauthorized",
		http.StatusForbidden:           "Forbidden",
		http.StatusConflict:            "Conflict",
		http.StatusUnprocessableEntity: "Invalid",
		http.StatusMethodNotAllowed:    "MethodNotAllowed",
		http.StatusInternalServerError: "InternalError",
		http.StatusTeapot:              "", // no well-known reason
		// Deliberately excluded, not just missing: a plain 404 must NOT get
		// reason: "NotFound" — see statusReasonForCode's doc comment (#177).
		http.StatusNotFound: "",
	}
	for code, want := range cases {
		if got := statusReasonForCode(code); got != want {
			t.Errorf("statusReasonForCode(%d) = %q, want %q", code, got, want)
		}
	}
}
