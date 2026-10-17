package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) collectManagementV8RouteKeys(out map[string]struct{}) {
	for _, route := range s.engine.Routes() {
		if strings.HasPrefix(route.Path, "/v8/management/") {
			out[route.Method+" "+route.Path] = struct{}{}
		}
	}
}

func (s *Server) pluginManagementV8NoRoute(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	if s.pluginHost == nil || s.mgmt == nil || !s.managementRoutesRegistered.Load() {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if !s.managementAvailable(c) {
		return
	}
	s.mgmt.Middleware()(c)
	if c.IsAborted() {
		return
	}
	if s.pluginHost.ServeManagementV8HTTP(c.Writer, c.Request) {
		c.Abort()
		return
	}
	c.AbortWithStatus(http.StatusNotFound)
}
