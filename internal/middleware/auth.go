package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const ctxUserID = "auth.user_id"

// Auth verifies the access token identify signs (HS256 with JWT_SECRET) and
// stores its subject. Kong checks it too, but billing moves money, so it
// doesn't trust a token it didn't verify.
func Auth(secret []byte) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok {
			unauthorized(c)
			return
		}
		var claims jwt.RegisteredClaims
		_, err := jwt.ParseWithClaims(strings.TrimSpace(raw), &claims, func(*jwt.Token) (any, error) {
			return secret, nil
		}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
		if err != nil {
			unauthorized(c)
			return
		}
		userID, err := uuid.Parse(claims.Subject)
		if err != nil {
			unauthorized(c)
			return
		}
		SetUserID(c, userID)
		c.Next()
	}
}

// SetUserID marks the request as made by userID. Auth calls it; handler
// tests use it to skip signing tokens.
func SetUserID(c *gin.Context, userID uuid.UUID) {
	c.Set(ctxUserID, userID)
}

// UserID returns the user Auth authenticated. Only call it behind Auth.
func UserID(c *gin.Context) uuid.UUID {
	id, _ := c.Get(ctxUserID)
	userID, _ := id.(uuid.UUID)
	return userID
}

func unauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{"code": "UNAUTHORIZED", "message": "La sesión expiró o ya no es válida"},
	})
}
