package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lejianwen/rustdesk-api/v2/service"
)

// AnyUserAuth (DareMoto) accepts the token of the desktop client ("Authorization: Bearer")
// and of the web pages ("api-token"), so the same billing endpoints serve both.
func AnyUserAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("api-token")
		if token == "" {
			if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
				token = strings.TrimSpace(h[7:])
			}
		}
		if token == "" {
			c.AbortWithStatusJSON(401, gin.H{"code": 401, "message": "Accesso richiesto"})
			return
		}
		user, ut := service.AllService.UserService.InfoByAccessToken(token)
		if user.Id == 0 || !service.AllService.UserService.CheckUserEnable(user) {
			c.AbortWithStatusJSON(401, gin.H{"code": 401, "message": "Accesso richiesto"})
			return
		}
		c.Set("curUser", user)
		c.Set("token", token)
		service.AllService.UserService.AutoRefreshAccessToken(ut)
		c.Next()
	}
}
