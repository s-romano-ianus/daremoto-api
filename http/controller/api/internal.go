package api

// DareMoto: endpoints for the DareMoto servers on the same host (not for clients).
// Protected by the shared secret DAREMOTO_INTERNAL_SECRET (header X-DareMoto-Secret);
// disabled when the secret is not set. Caddy also blocks /api/internal/ from outside.

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/service"
)

type Internal struct{}

type authorizeForm struct {
	Token    string `json:"token"`
	TargetId string `json:"target_id"`
}

func internalAllowed(c *gin.Context) bool {
	secret := strings.TrimSpace(os.Getenv("DAREMOTO_INTERNAL_SECRET"))
	got := c.GetHeader("X-DareMoto-Secret")
	return secret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(got)) == 1
}

// Authorize [POST] /api/internal/authorize {token, target_id} -> {allow, message}
func (i *Internal) Authorize(c *gin.Context) {
	if !internalAllowed(c) {
		c.String(http.StatusNotFound, "404 not found")
		return
	}
	f := &authorizeForm{}
	if err := c.ShouldBindJSON(f); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"allow": false, "message": "bad request"})
		return
	}
	c.JSON(http.StatusOK, service.Sessions.Authorize(strings.TrimSpace(f.Token), strings.TrimSpace(f.TargetId)))
}
