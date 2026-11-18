package apierr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

var codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// assertEnvelope verifica la forma completa del sobre de RF1.
func assertEnvelope(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", w.Code, status, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("body no es JSON: %q", w.Body)
	}
	if len(top) != 1 || top["error"] == nil {
		t.Fatalf("la única clave de primer nivel debe ser error: %s", w.Body)
	}
	var e struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(top["error"], &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != code || !codePattern.MatchString(e.Code) {
		t.Errorf("code = %q, want %q", e.Code, code)
	}
	if e.Message == "" {
		t.Error("message vacío")
	}
	if e.Details == nil {
		t.Errorf("details debe ser un objeto: %s", top["error"])
	}
	return e.Details
}

func serve(r *gin.Engine, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestAbortNilDetailsIsEmptyObject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", func(c *gin.Context) { Abort(c, http.StatusTeapot, "SOME_CODE", "mensaje", nil) })

	details := assertEnvelope(t, serve(r, http.MethodGet, "/x"), http.StatusTeapot, "SOME_CODE")
	if len(details) != 0 {
		t.Errorf("details = %v, want {}", details)
	}
}

func TestWrappers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/query", func(c *gin.Context) { InvalidQuery(c, "limit", "limit inválido") })
	r.GET("/body", func(c *gin.Context) { InvalidBody(c, "body inválido", nil) })
	r.GET("/big", func(c *gin.Context) { PayloadTooLarge(c, "muy grande", 65536) })

	if d := assertEnvelope(t, serve(r, http.MethodGet, "/query"), http.StatusBadRequest, CodeInvalidQuery); d["param"] != "limit" {
		t.Errorf("details = %v", d)
	}
	assertEnvelope(t, serve(r, http.MethodGet, "/body"), http.StatusBadRequest, CodeInvalidBody)
	if d := assertEnvelope(t, serve(r, http.MethodGet, "/big"), http.StatusRequestEntityTooLarge, CodePayloadTooLarge); d["max_bytes"] != float64(65536) {
		t.Errorf("details = %v", d)
	}
}

func TestFallbacks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterFallbacks(r)
	r.GET("/only-get", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	assertEnvelope(t, serve(r, http.MethodGet, "/nope"), http.StatusNotFound, CodeNotFound)
	assertEnvelope(t, serve(r, http.MethodPost, "/only-get"), http.StatusMethodNotAllowed, CodeMethodNotAllowed)
}
