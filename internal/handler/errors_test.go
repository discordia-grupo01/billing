package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/discordia-grupo01/billing/internal/apierr"
)

var errorCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// assertEnvelopeShape checks the whole error contract: a single "error" key
// with a SCREAMING_SNAKE code, a non-empty message and details as an object.
func assertEnvelopeShape(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("body is not JSON: %q", w.Body)
	}
	if len(top) != 1 || top["error"] == nil {
		t.Fatalf("error must be the only top-level key: %s", w.Body)
	}
	var e struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(top["error"], &e); err != nil {
		t.Fatal(err)
	}
	if !errorCodePattern.MatchString(e.Code) {
		t.Errorf("code %q is not SCREAMING_SNAKE", e.Code)
	}
	if e.Message == "" {
		t.Error("empty message")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(e.Details)), "{") {
		t.Errorf("details must be an object: %s", w.Body)
	}
}

func TestMalformedJSONIsInvalidBodyWithEmptyDetails(t *testing.T) {
	w := do(newRouter(&fakePoints{}, &fakePremium{}), http.MethodPost, pointsCheckoutPath, `{`)

	assertError(t, w, http.StatusBadRequest, apierr.CodeInvalidBody)
	if d := decode[apierr.ErrorResponse](t, w).Error.Details; len(d) != 0 {
		t.Errorf("details = %v, want {}", d)
	}
}

func TestErrorsWithoutDetailsStillSendThem(t *testing.T) {
	w := do(newRouter(&fakePoints{err: errBoom}, &fakePremium{}), http.MethodGet, "/v1/billing/points", "")

	assertError(t, w, http.StatusInternalServerError, apierr.CodeInternal)
	if !strings.Contains(w.Body.String(), `"details":{}`) {
		t.Errorf("details must be {}: %s", w.Body)
	}
	if strings.Contains(w.Body.String(), errBoom.Error()) {
		t.Errorf("internal error leaked: %s", w.Body)
	}
}

func TestUnknownRouteAndMethodUseTheEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := newRouter(&fakePoints{}, &fakePremium{})
	apierr.RegisterFallbacks(r)

	assertError(t, do(r, http.MethodGet, "/v1/billing/nope", ""), http.StatusNotFound, apierr.CodeNotFound)
	assertError(t, do(r, http.MethodDelete, "/v1/billing/points/packages", ""), http.StatusMethodNotAllowed, apierr.CodeMethodNotAllowed)
}
