// Package cachepolicy validates the host-owned part of the Pacer v1 contract.
package cachepolicy

import (
	"encoding/base64"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const Path = "/v1/plugins/pacer/cache-policy"
const HeaderPrefix = "X-Pacer-Cache-"
const CapabilityHeader = HeaderPrefix + "Capability"

var identifier = regexp.MustCompile(`^[!-~]{1,256}$`)
var callerDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Error struct {
	Code   string
	Status int
}

func (e *Error) Error() string         { return e.Code }
func (e *Error) StatusCode() int       { return e.Status }
func (e *Error) IsRequestScoped() bool { return true }
func Invalid() error                   { return &Error{"pacer_cache_invalid_request", http.StatusBadRequest} }
func Unavailable() error {
	return &Error{"pacer_cache_policy_unavailable", http.StatusServiceUnavailable}
}
func Unsupported() error { return &Error{"pacer_cache_unsupported", http.StatusUnprocessableEntity} }

func ValidID(s string) bool { return identifier.MatchString(s) }
func ValidCapability(s string) bool {
	v, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(v) == 32 && base64.RawURLEncoding.EncodeToString(v) == s
}
func ValidScope(s pluginapi.CachePolicyScope) bool {
	return callerDigest.MatchString(s.CallerScope) && ValidID(s.InstallationID) && ValidID(s.SessionID) && ValidID(s.ProviderRoute)
}
func Managed(h http.Header) bool {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), strings.ToLower(HeaderPrefix)) {
			return true
		}
	}
	return false
}

// ParseHeaders rejects partial, duplicate and unknown managed headers. It does not authenticate ownership.
func ParseHeaders(h http.Header, caller string) (pluginapi.CachePolicyResolveRequest, error) {
	var r pluginapi.CachePolicyResolveRequest
	v := make(map[string]string)
	for k, values := range h {
		if !strings.HasPrefix(strings.ToLower(k), strings.ToLower(HeaderPrefix)) {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(strings.ToLower(k), strings.ToLower(HeaderPrefix)))
		switch name {
		case "protocol", "installation", "session", "route", "epoch", "revision", "kind", "capability":
		default:
			return r, Invalid()
		}
		if len(values) != 1 || v[name] != "" || values[0] == "" {
			return r, Invalid()
		}
		v[name] = values[0]
	}
	if v["protocol"] != "1" {
		return r, Invalid()
	}
	r.Protocol = 1
	r.Scope = pluginapi.CachePolicyScope{CallerScope: caller, InstallationID: v["installation"], SessionID: v["session"], ProviderRoute: v["route"]}
	r.ContextEpoch, r.Kind, r.Capability = v["epoch"], v["kind"], v["capability"]
	var err error
	r.Revision, err = strconv.ParseInt(v["revision"], 10, 64)
	if err != nil || r.Revision < 1 || !ValidScope(r.Scope) || !ValidID(r.ContextEpoch) || !ValidCapability(r.Capability) || (r.Kind != "primary" && r.Kind != "warming") {
		return r, Invalid()
	}
	return r, nil
}

func Validate(p pluginapi.CachePolicy) error {
	if p.Protocol != 1 || !ValidScope(p.Scope) || !ValidID(p.ContextEpoch) || p.Revision < 1 || (p.TTL != "5m" && p.TTL != "1h") || (p.Kind != "primary" && p.Kind != "warming") {
		return Invalid()
	}
	return nil
}

func Matches(p pluginapi.CachePolicy, r pluginapi.CachePolicyResolveRequest) bool {
	return Validate(p) == nil && p.Scope == r.Scope && p.ContextEpoch == r.ContextEpoch && p.Revision == r.Revision && p.Kind == r.Kind
}

func Clone(p *pluginapi.CachePolicy) *pluginapi.CachePolicy {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func CloneEvidence(p *pluginapi.CachePolicyEvidence) *pluginapi.CachePolicyEvidence {
	if p == nil {
		return nil
	}
	v := *p
	v.Selected = Clone(p.Selected)
	v.Prefix = clonePrefix(p.Prefix)
	return &v
}

// clonePrefix deep-copies slices so consumers cannot alias the evidence held by the executor.
func clonePrefix(p *pluginapi.CachePrefixSnapshot) *pluginapi.CachePrefixSnapshot {
	if p == nil {
		return nil
	}
	v := *p
	v.Endpoints = append([]pluginapi.CachePrefixEndpoint{}, p.Endpoints...)
	if p.Reasons != nil {
		v.Reasons = append([]string{}, p.Reasons...)
	}
	return &v
}
