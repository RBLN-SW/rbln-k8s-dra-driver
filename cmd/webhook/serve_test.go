package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/RBLN-SW/k8s-dra-driver-npu/internal/logtest"
	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
)

// admissionReviewBody builds a decodable AdmissionReview carrying marker as the
// reviewed object's name, so a payload dump is detectable in the log stream.
func admissionReviewBody(t *testing.T, marker string) []byte {
	t.Helper()
	ar := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Name:      marker,
			Namespace: "default",
			Resource: metav1.GroupVersionResource{
				Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims",
			},
		},
	}
	data, err := json.Marshal(ar)
	if err != nil {
		t.Fatalf("marshal AdmissionReview: %v", err)
	}
	return data
}

func postAdmissionReview(t *testing.T, body []byte, admit func(context.Context, admissionv1.AdmissionReview) *admissionv1.AdmissionResponse) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/validate-resource-claim-parameters", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	serve(rec, req, admit)
	return rec
}

func allowAll(context.Context, admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{Allowed: true}
}

// Admission payloads carry arbitrary user object contents, so they belong at
// the trace gate only — debug is documented as production-usable.
//
// The assertion is on the "body" attr of the dump record, not on a substring of
// the whole stream: the reviewed object's name is a correlation key on every
// record from debug up, so searching the buffer for it would report a payload
// dump that never happened.
func TestServeDumpsAdmissionPayloadOnlyAtTrace(t *testing.T) {
	const marker = "payload-marker-claim"
	for _, tc := range []struct {
		gate     string
		wantDump bool
	}{
		{"info", false},
		{"debug", false},
		{"trace", true},
	} {
		buf := logtest.Capture(t, tc.gate)

		rec := postAdmissionReview(t, admissionReviewBody(t, marker), allowAll)

		if rec.Code != http.StatusOK {
			t.Errorf("gate=%s: status = %d, body = %s", tc.gate, rec.Code, rec.Body.String())
		}
		dump := logtest.Find(logtest.Lines(t, buf), "Handling admission request")
		if got := dump != nil; got != tc.wantDump {
			t.Errorf("gate=%s: payload dumped = %v, want %v: %s", tc.gate, got, tc.wantDump, buf.String())
			continue
		}
		if !tc.wantDump {
			continue
		}
		body, ok := dump["body"].(string)
		if !ok || !strings.Contains(body, marker) {
			t.Errorf("gate=%s: dump record carries no raw body: %v", tc.gate, dump)
		}
	}
}

// A rejection is the only thing a webhook operator is ever paged about, and it
// is useless without knowing which object was rejected. The keys must come from
// the request context so every record on the path carries them, not just the
// ones whose call site remembered to pass them.
func TestServeAttachesRequestIdentityToEveryRecord(t *testing.T) {
	buf := logtest.Capture(t, "debug")

	reject := func(ctx context.Context, _ admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
		logging.FromContext(ctx).Warn("Rejected resource claim parameters", "err", "bad request")
		return &admissionv1.AdmissionResponse{Result: &metav1.Status{Message: "bad request"}}
	}
	postAdmissionReview(t, admissionReviewBody(t, "claim-a"), reject)

	lines := logtest.Lines(t, buf)
	for _, msg := range []string{"Rejected resource claim parameters", "Sending admission response"} {
		line := logtest.Find(lines, msg)
		if line == nil {
			t.Fatalf("%q not logged: %s", msg, buf.String())
		}
		if line["requestUID"] != "test-uid" {
			t.Errorf("%q: requestUID = %v, want test-uid", msg, line["requestUID"])
		}
		if line["namespace"] != "default" || line["name"] != "claim-a" {
			t.Errorf("%q: namespace/name = %v/%v, want default/claim-a", msg, line["namespace"], line["name"])
		}
		if line["resource"] == nil {
			t.Errorf("%q: resource missing", msg)
		}
	}
}

// An AdmissionReview with no request decodes cleanly, and the response path
// dereferences Request.UID. Panicking there costs the contract twice: net/http
// writes an unstructured stack trace, and the apiserver sees a bare 500.
func TestServeRejectsReviewWithoutRequest(t *testing.T) {
	buf := logtest.Capture(t, "info")

	body, err := json.Marshal(admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
	})
	if err != nil {
		t.Fatalf("marshal AdmissionReview: %v", err)
	}

	rec := postAdmissionReview(t, body, allowAll)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	line := logtest.Find(logtest.Lines(t, buf), "Failed to read AdmissionReview from request body")
	if line == nil {
		t.Fatalf("malformed review was not logged: %s", buf.String())
	}
	if line["level"] != "warn" {
		t.Errorf("level = %v, want warn", line["level"])
	}
}
