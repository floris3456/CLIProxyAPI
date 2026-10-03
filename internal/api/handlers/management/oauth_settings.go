package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// oauth-settings: map[string][]OAuthModelSetting (per-channel model overrides for OAuth credentials).
//
// Writes are validated (unknown thinking levels and negative limits are rejected instead of
// silently dropped) and entries without any override are removed, so the stored config only
// holds settings that change something.

// GetOAuthSettings returns the configured per-channel model settings.
func (h *Handler) GetOAuthSettings(c *gin.Context) {
	h.mu.Lock()
	settings := cloneOAuthSettings(h.cfg.OAuthSettings)
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"oauth-settings": settings})
}

// PutOAuthSettings replaces all per-channel model settings.
func (h *Handler) PutOAuthSettings(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]config.OAuthModelSetting
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]config.OAuthModelSetting `json:"items"`
		}
		if err2 := json.Unmarshal(data, &wrapper); err2 != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		entries = wrapper.Items
	}
	if _, isWrapper := entries["items"]; isWrapper && len(entries) == 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	next := make(map[string][]config.OAuthModelSetting, len(entries))
	for rawChannel, list := range entries {
		channel := normalizeOAuthSettingsChannel(rawChannel)
		if channel == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel"})
			return
		}
		clean, errValidate := validateOAuthSettingList(list)
		if errValidate != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error(), "channel": channel})
			return
		}
		if len(clean) > 0 {
			next[channel] = append(next[channel], clean...)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.OAuthSettings = sanitizedOAuthSettings(next)
	h.persistLocked(c)
}

// PatchOAuthSettings updates one channel. Body forms:
//
//	{"channel": "codex", "settings": [...]}  replaces the channel's list (empty removes the channel)
//	{"channel": "codex", "setting": {...}}   adds or replaces one model entry (matched by name and
//	                                         alias); an entry without overrides removes that model
func (h *Handler) PatchOAuthSettings(c *gin.Context) {
	var body struct {
		Channel  *string                     `json:"channel"`
		Provider *string                     `json:"provider"`
		Settings *[]config.OAuthModelSetting `json:"settings"`
		Setting  *config.OAuthModelSetting   `json:"setting"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	channelRaw := ""
	if body.Channel != nil {
		channelRaw = *body.Channel
	} else if body.Provider != nil {
		channelRaw = *body.Provider
	}
	channel := normalizeOAuthSettingsChannel(channelRaw)
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel"})
		return
	}
	if (body.Settings == nil) == (body.Setting == nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "exactly one of settings or setting is required"})
		return
	}

	if body.Settings != nil {
		clean, errValidate := validateOAuthSettingList(*body.Settings)
		if errValidate != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		next := cloneOAuthSettings(h.cfg.OAuthSettings)
		if len(clean) == 0 {
			if _, ok := next[channel]; !ok {
				c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
				return
			}
			delete(next, channel)
		} else {
			if next == nil {
				next = make(map[string][]config.OAuthModelSetting)
			}
			next[channel] = clean
		}
		h.cfg.OAuthSettings = sanitizedOAuthSettings(next)
		h.persistLocked(c)
		return
	}

	entry, errValidate := validateOAuthSetting(*body.Setting)
	if errValidate != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	next := cloneOAuthSettings(h.cfg.OAuthSettings)
	if next == nil {
		next = make(map[string][]config.OAuthModelSetting)
	}
	list := next[channel]
	kept := make([]config.OAuthModelSetting, 0, len(list)+1)
	replaced := false
	for _, existing := range list {
		if sameOAuthSettingTarget(existing, entry) {
			if entry.HasOverrides() && !replaced {
				kept = append(kept, entry)
			}
			replaced = true
			continue
		}
		kept = append(kept, existing)
	}
	if !replaced {
		if !entry.HasOverrides() {
			c.JSON(http.StatusNotFound, gin.H{"error": "model setting not found"})
			return
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		delete(next, channel)
	} else {
		next[channel] = kept
	}
	h.cfg.OAuthSettings = sanitizedOAuthSettings(next)
	h.persistLocked(c)
}

// DeleteOAuthSettings removes a channel (?channel=codex) or one model entry
// (?channel=codex&name=gpt-5.5[&alias=...]).
func (h *Handler) DeleteOAuthSettings(c *gin.Context) {
	channel := normalizeOAuthSettingsChannel(c.Query("channel"))
	if channel == "" {
		channel = normalizeOAuthSettingsChannel(c.Query("provider"))
	}
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing channel"})
		return
	}
	name := strings.TrimSpace(c.Query("name"))
	alias := strings.TrimSpace(c.Query("alias"))
	h.mu.Lock()
	defer h.mu.Unlock()
	next := cloneOAuthSettings(h.cfg.OAuthSettings)
	list, ok := next[channel]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	if name == "" {
		delete(next, channel)
	} else {
		target := config.OAuthModelSetting{Name: name, Alias: alias}
		kept := make([]config.OAuthModelSetting, 0, len(list))
		for _, existing := range list {
			if !sameOAuthSettingTarget(existing, target) {
				kept = append(kept, existing)
			}
		}
		if len(kept) == len(list) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model setting not found"})
			return
		}
		if len(kept) == 0 {
			delete(next, channel)
		} else {
			next[channel] = kept
		}
	}
	h.cfg.OAuthSettings = sanitizedOAuthSettings(next)
	h.persistLocked(c)
}

// oauthSettingsModel is one model served through an OAuth channel, with the limits CPA
// currently reports for it and the configured setting that applies (if any).
type oauthSettingsModel struct {
	Channel             string                    `json:"channel"`
	ID                  string                    `json:"id"`
	Name                string                    `json:"name"`
	DisplayName         string                    `json:"display_name,omitempty"`
	Kind                string                    `json:"kind"`
	ContextLength       int                       `json:"context_length,omitempty"`
	InputLength         int                       `json:"input_length,omitempty"`
	MaxCompletionTokens int                       `json:"max_completion_tokens,omitempty"`
	Reasoning           *oauthSettingsReasoning   `json:"reasoning,omitempty"`
	Setting             *config.OAuthModelSetting `json:"setting"`
}

type oauthSettingsReasoning struct {
	Mode   string   `json:"mode"`
	Levels []string `json:"levels,omitempty"`
}

// detailEntry mirrors the fields of the details catalogue this view needs.
type detailEntry struct {
	ID                  string                  `json:"id"`
	DisplayName         string                  `json:"display_name"`
	Kind                string                  `json:"kind"`
	ContextLength       int                     `json:"context_length"`
	InputLength         int                     `json:"input_length"`
	MaxCompletionTokens int                     `json:"max_completion_tokens"`
	Reasoning           *oauthSettingsReasoning `json:"reasoning"`
}

// GetOAuthSettingsModels lists the models of every OAuth channel with their current
// (effective) limits, the configured settings and the thinking level names CPA accepts.
func (h *Handler) GetOAuthSettingsModels(c *gin.Context) {
	h.mu.Lock()
	settings := cloneOAuthSettings(h.cfg.OAuthSettings)
	manager := h.authManager
	detailsProvider := h.modelDetails
	h.mu.Unlock()

	details := map[string]detailEntry{}
	hash := ""
	if detailsProvider != nil {
		doc := detailsProvider()
		hash, _ = doc["hash"].(string)
		if encoded, errMarshal := json.Marshal(doc["data"]); errMarshal == nil {
			var list []detailEntry
			if errUnmarshal := json.Unmarshal(encoded, &list); errUnmarshal == nil {
				for _, entry := range list {
					details[entry.ID] = entry
				}
			}
		}
	}

	channels := map[string]bool{}
	for channel := range settings {
		channels[channel] = true
	}
	seen := map[string]bool{}
	models := make([]oauthSettingsModel, 0)
	if manager != nil {
		modelRegistry := registry.GetGlobalRegistry()
		for _, auth := range manager.List() {
			if auth == nil || auth.Disabled {
				continue
			}
			channel := coreauth.OAuthModelAliasChannel(auth.Provider, auth.AuthKind())
			if channel == "" {
				continue
			}
			channels[channel] = true
			prefix := strings.Trim(strings.TrimSpace(auth.Prefix), "/")
			for _, info := range modelRegistry.GetModelsForClient(auth.ID) {
				if info == nil || strings.TrimSpace(info.ID) == "" {
					continue
				}
				key := channel + "\x00" + info.ID
				if seen[key] {
					continue
				}
				seen[key] = true
				name := info.ID
				if prefix != "" {
					name = strings.TrimPrefix(name, prefix+"/")
				}
				model := oauthSettingsModel{
					Channel:             channel,
					ID:                  info.ID,
					Name:                name,
					DisplayName:         info.DisplayName,
					Kind:                "chat",
					ContextLength:       info.ContextLength,
					MaxCompletionTokens: info.MaxCompletionTokens,
				}
				if info.MaxContextLength > 0 {
					model.ContextLength = info.MaxContextLength
				}
				if info.Type == registry.OpenAIImageModelType {
					model.Kind = "image"
				}
				if detail, ok := details[info.ID]; ok {
					model.DisplayName = detail.DisplayName
					model.Kind = detail.Kind
					model.ContextLength = detail.ContextLength
					model.InputLength = detail.InputLength
					model.MaxCompletionTokens = detail.MaxCompletionTokens
					model.Reasoning = detail.Reasoning
				}
				if setting := config.ResolveOAuthModelSetting(settings[channel], name, info.MetadataModelID, info.Name); setting != nil {
					copied := *setting
					copied.ThinkingLevels = append([]string(nil), setting.ThinkingLevels...)
					model.Setting = &copied
				}
				models = append(models, model)
			}
		}
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Channel != models[j].Channel {
			return models[i].Channel < models[j].Channel
		}
		return models[i].ID < models[j].ID
	})
	channelList := make([]string, 0, len(channels))
	for channel := range channels {
		channelList = append(channelList, channel)
	}
	sort.Strings(channelList)
	if settings == nil {
		settings = map[string][]config.OAuthModelSetting{}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"channels":        channelList,
		"models":          models,
		"oauth-settings":  settings,
		"thinking-levels": append([]string(nil), config.OAuthSettingThinkingLevels...),
		"details-hash":    hash,
	})
}

func normalizeOAuthSettingsChannel(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// validateOAuthSetting trims an entry and rejects values CPA would ignore or misread.
func validateOAuthSetting(entry config.OAuthModelSetting) (config.OAuthModelSetting, error) {
	entry.Name = strings.TrimSpace(entry.Name)
	entry.Alias = strings.TrimSpace(entry.Alias)
	entry.DisplayName = strings.TrimSpace(entry.DisplayName)
	if entry.Name == "" {
		return entry, fmt.Errorf("model name is required")
	}
	if entry.MaxContextLength < 0 {
		return entry, fmt.Errorf("%s: max-context-length must not be negative", entry.Name)
	}
	if entry.MaxOutputTokens < 0 {
		return entry, fmt.Errorf("%s: max-output-tokens must not be negative", entry.Name)
	}
	levels, unknown := config.NormalizeOAuthSettingThinkingLevels(entry.ThinkingLevels)
	if len(unknown) > 0 {
		return entry, fmt.Errorf("%s: unknown thinking levels %v (known: %v)", entry.Name, unknown, config.OAuthSettingThinkingLevels)
	}
	entry.ThinkingLevels = levels
	return entry, nil
}

// validateOAuthSettingList validates every entry and drops entries that change nothing.
func validateOAuthSettingList(list []config.OAuthModelSetting) ([]config.OAuthModelSetting, error) {
	out := make([]config.OAuthModelSetting, 0, len(list))
	for _, raw := range list {
		entry, err := validateOAuthSetting(raw)
		if err != nil {
			return nil, err
		}
		if entry.HasOverrides() {
			out = append(out, entry)
		}
	}
	return out, nil
}

func sameOAuthSettingTarget(a, b config.OAuthModelSetting) bool {
	return strings.EqualFold(strings.TrimSpace(a.Name), strings.TrimSpace(b.Name)) &&
		strings.EqualFold(strings.TrimSpace(a.Alias), strings.TrimSpace(b.Alias))
}

func cloneOAuthSettings(in map[string][]config.OAuthModelSetting) map[string][]config.OAuthModelSetting {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]config.OAuthModelSetting, len(in))
	for channel, list := range in {
		copied := make([]config.OAuthModelSetting, len(list))
		for i, entry := range list {
			entry.ThinkingLevels = append([]string(nil), entry.ThinkingLevels...)
			copied[i] = entry
		}
		out[channel] = copied
	}
	return out
}

func sanitizedOAuthSettings(in map[string][]config.OAuthModelSetting) map[string][]config.OAuthModelSetting {
	if len(in) == 0 {
		return nil
	}
	cfg := config.Config{OAuthSettings: in}
	cfg.SanitizeOAuthSettings()
	if len(cfg.OAuthSettings) == 0 {
		return nil
	}
	return cfg.OAuthSettings
}
