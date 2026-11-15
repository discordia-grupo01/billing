package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func serveHealth(t *testing.T, path string, h gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(path, h)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestLivezAnswersOK(t *testing.T) {
	rec := serveHealth(t, "/livez", Livez)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestReadyzReportsTheDatabase(t *testing.T) {
	ready := serveHealth(t, "/readyz", Readyz(fakePinger{}))
	if ready.Code != http.StatusOK || ready.Body.String() != `{"status":"ready"}` {
		t.Fatalf("ready: got %d %q", ready.Code, ready.Body.String())
	}
	down := serveHealth(t, "/readyz", Readyz(fakePinger{err: errors.New("down")}))
	if down.Code != http.StatusServiceUnavailable || down.Body.String() != `{"status":"unavailable"}` {
		t.Fatalf("down: got %d %q", down.Code, down.Body.String())
	}
}
