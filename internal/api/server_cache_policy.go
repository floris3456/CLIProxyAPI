package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func (s *Server) pacerCachePolicy(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	principal, _ := c.Get("userApiKey")
	caller, _ := principal.(string)
	var err error
	req, err := cachepolicy.DecodeHTTP(c.Request, session.CallerScope(caller))
	if err == nil && s.pluginHost == nil {
		err = cachepolicy.Unavailable()
	}
	if err == nil {
		resp, errHandle := s.pluginHost.HandleCachePolicy(c.Request.Context(), req)
		if errHandle == nil {
			c.Header("X-Pacer-Cache-Protocol", "1")
			c.Header("X-Pacer-Cache-Contract-Revision", "2")
			c.Header("X-Pacer-Cache-Attempts", "begin-validate-finish-v1")
			c.Data(resp.StatusCode, "application/json", resp.Body)
			return
		}
		err = errHandle
	}
	status := http.StatusServiceUnavailable
	if coded, ok := err.(interface{ StatusCode() int }); ok {
		status = coded.StatusCode()
	}
	if status == http.StatusMethodNotAllowed {
		c.Header("Allow", "GET, POST")
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": err.Error()}})
}
