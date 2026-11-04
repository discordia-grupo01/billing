package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	secret = []byte("test-secret")
	userID = uuid.MustParse("11111111-1111-1111-1111-111111111111")
)

func sign(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{"sub": userID.String(), "exp": time.Now().Add(time.Minute).Unix()}
}

func serve(header string) (*httptest.ResponseRecorder, uuid.UUID) {
	gin.SetMode(gin.TestMode)
	var got uuid.UUID
	r := gin.New()
	r.GET("/", Auth(secret), func(c *gin.Context) {
		got = UserID(c)
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, got
}

func TestAuthAcceptsValidToken(t *testing.T) {
	w, got := serve("Bearer " + sign(t, jwt.SigningMethodHS256, secret, validClaims()))

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got != userID {
		t.Errorf("user id = %s, want %s", got, userID)
	}
}

func TestAuthRejects(t *testing.T) {
	expired := validClaims()
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	noExp := validClaims()
	delete(noExp, "exp")
	badSub := validClaims()
	badSub["sub"] = "not-a-uuid"

	tests := map[string]string{
		"no header":        "",
		"not bearer":       "Basic abc",
		"garbage":          "Bearer abc.def.ghi",
		"wrong secret":     "Bearer " + sign(t, jwt.SigningMethodHS256, []byte("other"), validClaims()),
		"wrong algorithm":  "Bearer " + sign(t, jwt.SigningMethodHS512, secret, validClaims()),
		"expired":          "Bearer " + sign(t, jwt.SigningMethodHS256, secret, expired),
		"without exp":      "Bearer " + sign(t, jwt.SigningMethodHS256, secret, noExp),
		"subject not uuid": "Bearer " + sign(t, jwt.SigningMethodHS256, secret, badSub),
	}
	for name, header := range tests {
		t.Run(name, func(t *testing.T) {
			w, _ := serve(header)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
		})
	}
}

func TestUserIDWithoutAuth(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got := UserID(c); got != uuid.Nil {
		t.Errorf("UserID = %s, want Nil", got)
	}
}
