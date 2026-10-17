package pluginhost

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	managementV8PluginPrefix = "/v8/management/plugins/"
	managementV8MinSchema    = 7
	managementV8PathLimit    = 2048
	managementV8BodyLimit    = 64 << 10
	managementV8OutputLimit  = 8 << 20
)

// Malformed v8 declarations must never fall through to legacy normalization.
func declaresManagementV8(path string) bool {
	path = strings.TrimLeft(strings.TrimSpace(path), "/")
	if path == "v8" {
		return true
	}
	return len(path) > 2 && strings.HasPrefix(path, "v8") && strings.ContainsRune("/?#%\\", rune(path[2]))
}

func managementV8Segment(segment string) bool {
	if segment == "" {
		return false
	}
	for _, c := range segment {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func pluginManagementV8Base(record capabilityRecord) string {
	if record.plugin.SchemaVersion < managementV8MinSchema || !managementV8Segment(record.id) {
		return ""
	}
	base := managementV8PluginPrefix + record.id
	if len(base)+2 > managementV8PathLimit {
		return ""
	}
	return base
}

func managementV8LiteralPath(path string) bool {
	if len(path) > managementV8PathLimit || !strings.HasPrefix(path, managementV8PluginPrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, managementV8PluginPrefix), "/")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !managementV8Segment(part) {
			return false
		}
	}
	return true
}

func managementV8Method(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// Gin built-ins own their entire literal/pattern path, including other verbs.
// This also reserves future :parameter and *catch-all built-ins without aliases.
func managementV8Reserved(path string, reserved map[string]struct{}) bool {
	for key := range reserved {
		_, pattern, ok := strings.Cut(key, " ")
		if !ok || !strings.HasPrefix(pattern, "/v8/management/") {
			continue
		}
		want, got := strings.Split(pattern, "/"), strings.Split(path, "/")
		matched := true
		for i, part := range want {
			if strings.HasPrefix(part, "*") {
				if i < len(got) {
					return true
				}
				matched = false
				break
			}
			if i >= len(got) || !(part == got[i] || strings.HasPrefix(part, ":") && got[i] != "") {
				matched = false
				break
			}
		}
		if matched && len(want) == len(got) {
			return true
		}
	}
	return false
}

func registerManagementV8Route(routes map[string]managementRouteRecord, reserved map[string]struct{}, record capabilityRecord, item pluginapi.ManagementRoute) {
	base := pluginManagementV8Base(record)
	if base == "" || item.Handler == nil || item.Menu != "" || !managementV8Method(item.Method) ||
		!managementV8LiteralPath(item.Path) || !strings.HasPrefix(item.Path, base+"/") || managementV8Reserved(item.Path, reserved) {
		return
	}
	key := managementRouteKey(item.Method, item.Path)
	if _, exists := routes[key]; exists {
		return // Same first/higher-priority declaration wins as legacy registration.
	}
	routes[key] = managementRouteRecord{pluginID: record.id, path: record.path, version: record.version, schemaVersion: record.plugin.SchemaVersion, route: item}
}

func managementV8Error(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Error(w, message, status)
}

func managementV8HeaderValue(value string) bool {
	if len(value) > 8192 {
		return false
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

// ServeManagementV8HTTP requires the caller to authenticate management access.
// It never performs legacy dispatch, resource conversion or auth-URL fallback.
func (h *Host) ServeManagementV8HTTP(w http.ResponseWriter, r *http.Request) bool {
	if h == nil || w == nil || r == nil || r.URL == nil {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !managementV8Method(r.Method) || !managementV8LiteralPath(r.URL.Path) ||
		r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path || r.URL.Fragment != "" || r.URL.Opaque != "" {
		return false
	}
	h.mu.Lock()
	record, found := h.managementRoutes[managementRouteKey(r.Method, r.URL.Path)]
	h.mu.Unlock()
	if !found || record.route.Handler == nil || record.schemaVersion < managementV8MinSchema || h.isPluginFused(record.pluginID) ||
		!h.pluginIdentityCurrent(record.pluginID, record.path, record.version) {
		return false
	}
	if len(r.URL.RawQuery) > managementV8BodyLimit || r.ContentLength > managementV8BodyLimit {
		managementV8Error(w, http.StatusRequestEntityTooLarge, "plugin management request too large")
		return true
	}
	query, errQuery := url.ParseQuery(r.URL.RawQuery)
	if errQuery != nil {
		managementV8Error(w, http.StatusBadRequest, "invalid plugin management query")
		return true
	}
	var body []byte
	if r.Body != nil {
		var errRead error
		body, errRead = io.ReadAll(io.LimitReader(r.Body, managementV8BodyLimit+1))
		_ = r.Body.Close()
		if len(body) > managementV8BodyLimit {
			managementV8Error(w, http.StatusRequestEntityTooLarge, "plugin management request too large")
			return true
		}
		if errRead != nil {
			managementV8Error(w, http.StatusBadRequest, "failed to read plugin management request")
			return true
		}
	}
	headers := make(http.Header)
	for _, name := range []string{"Content-Type", "Accept", "If-Match"} {
		for _, value := range r.Header.Values(name) {
			if !managementV8HeaderValue(value) {
				managementV8Error(w, http.StatusBadRequest, "invalid plugin management header")
				return true
			}
			headers.Add(name, value)
		}
	}
	resp, errHandle := h.callManagementHandler(r.Context(), record, pluginapi.ManagementRequest{
		Method: r.Method, Path: r.URL.Path, Headers: headers, Query: query, Body: bytes.Clone(body),
	})
	if errHandle != nil {
		managementV8Error(w, http.StatusBadGateway, "plugin management handler failed")
		return true
	}
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	if len(resp.Body) > managementV8OutputLimit || status < 200 || status >= 600 || status >= 300 && status < 400 || status == http.StatusNoContent && len(resp.Body) != 0 {
		managementV8Error(w, http.StatusBadGateway, "invalid plugin management response")
		return true
	}
	// Minimal response allowlist. No redirects, cookies, authentication challenges,
	// cache overrides or private binding headers can be supplied by a plugin.
	contentType := resp.Headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	mediaType, _, errType := mime.ParseMediaType(contentType)
	if errType != nil || !managementV8HeaderValue(contentType) ||
		!(mediaType == "application/json" || mediaType == "application/problem+json" || mediaType == "text/csv" || mediaType == "application/octet-stream") {
		managementV8Error(w, http.StatusBadGateway, "invalid plugin management response")
		return true
	}
	disposition := resp.Headers.Get("Content-Disposition")
	if disposition != "" {
		kind, params, errDisposition := mime.ParseMediaType(disposition)
		if errDisposition != nil || kind != "attachment" || !managementV8HeaderValue(disposition) || len(params) != 1 || params["filename"] == "" ||
			strings.ContainsAny(params["filename"], "/\\") || !managementV8HeaderValue(params["filename"]) {
			managementV8Error(w, http.StatusBadGateway, "invalid plugin management response")
			return true
		}
		w.Header().Set("Content-Disposition", disposition)
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body) // Opaque schema-7 bytes: no JSON decode/re-encode/HTML escaping.
	return true
}
