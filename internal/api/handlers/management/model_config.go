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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Model catalogue for the management UI: every model CPA can serve from logged-in accounts
// (OAuth/plugin channels) and from OpenAI-compatible API-key providers, including disabled
// ones, with its exposed name (alias), current limits and settings. One PATCH applies a
// model's enabled state, alias and settings in a single config write.

const compatChannelPrefix = "compat:"

const (
	sourceAccount = "account"
	sourceAPIKey  = "api-key"
)

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

type modelDisabledBy struct {
	// Scope is "channel" (oauth-excluded-models), "account" (a credential's excluded_models)
	// or "provider" (an API-key provider's excluded-models).
	Scope   string `json:"scope"`
	Pattern string `json:"pattern,omitempty"`
	// Accounts is how many credentials exclude the model (scope "account").
	Accounts int `json:"accounts,omitempty"`
}

type modelAliasView struct {
	Name         string `json:"name"`
	KeepOriginal bool   `json:"keep-original,omitempty"`
}

type modelSupports struct {
	MaxOutputTokens bool `json:"max-output-tokens"`
	KeepOriginal    bool `json:"keep-original"`
}

// oauthSettingsModel is one upstream model of a channel or API-key provider.
type oauthSettingsModel struct {
	Channel  string `json:"channel"`
	Source   string `json:"source"`
	Upstream string `json:"upstream"`
	// ID is the primary model ID clients use (alias if set, with the credential prefix).
	ID string `json:"id"`
	// Name is ID without the credential prefix (what settings match).
	Name                string                    `json:"name"`
	Exposed             []string                  `json:"exposed"`
	Enabled             bool                      `json:"enabled"`
	DisabledBy          *modelDisabledBy          `json:"disabled_by,omitempty"`
	Alias               *modelAliasView           `json:"alias,omitempty"`
	Accounts            int                       `json:"accounts,omitempty"`
	DisplayName         string                    `json:"display_name,omitempty"`
	Kind                string                    `json:"kind"`
	ContextLength       int                       `json:"context_length,omitempty"`
	InputLength         int                       `json:"input_length,omitempty"`
	MaxCompletionTokens int                       `json:"max_completion_tokens,omitempty"`
	Reasoning           *oauthSettingsReasoning   `json:"reasoning,omitempty"`
	Setting             *config.OAuthModelSetting `json:"setting"`
	Supports            modelSupports             `json:"supports"`
}

type catalogueChannel struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Source string `json:"source"`
}

type modelCatalogue struct {
	channels []catalogueChannel
	models   []oauthSettingsModel
	// channelAuths are the enabled OAuth credentials of each channel.
	channelAuths map[string][]*coreauth.Auth
}

// buildModelCatalogue reads cfg and the auth manager; details may be nil (writes).
func buildModelCatalogue(cfg *config.Config, manager *coreauth.Manager, details map[string]detailEntry) modelCatalogue {
	cat := modelCatalogue{channelAuths: map[string][]*coreauth.Auth{}}
	channelSeen := map[string]bool{}
	addChannel := func(id, label, source string) {
		if !channelSeen[id] {
			channelSeen[id] = true
			cat.channels = append(cat.channels, catalogueChannel{ID: id, Label: label, Source: source})
		}
	}
	settings := cfg.OAuthSettings

	type rowState struct {
		model           *oauthSettingsModel
		enabledAccounts int
		accountExcluded int
		channelPattern  string
		accountPattern  string
	}
	rows := map[string]*rowState{}
	order := []string{}

	if manager != nil {
		modelRegistry := registry.GetGlobalRegistry()
		auths := manager.List()
		sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
		for _, auth := range auths {
			if auth == nil || auth.Disabled || isCompatAuth(auth) {
				continue
			}
			channel := coreauth.OAuthModelAliasChannel(auth.Provider, auth.AuthKind())
			if channel == "" {
				continue
			}
			addChannel(channel, channel, sourceAccount)
			cat.channelAuths[channel] = append(cat.channelAuths[channel], auth)
			prefix := strings.Trim(strings.TrimSpace(auth.Prefix), "/")
			global := cfg.OAuthExcludedModels[strings.ToLower(strings.TrimSpace(auth.Provider))]
			perAccount := authExcludedModels(auth)

			candidates := registry.ClientCandidates(auth.ID)
			if len(candidates) == 0 {
				for _, info := range modelRegistry.GetModelsForClient(auth.ID) {
					if info == nil {
						continue
					}
					id := info.ID
					if prefix != "" {
						id = strings.TrimPrefix(id, prefix+"/")
					}
					if info.MetadataModelID != "" {
						id = info.MetadataModelID
					}
					candidates = append(candidates, registry.CandidateModel{ID: id, DisplayName: info.DisplayName, Image: info.Type == registry.OpenAIImageModelType})
				}
			}
			for _, candidate := range candidates {
				key := channel + "\x00" + strings.ToLower(candidate.ID)
				state, ok := rows[key]
				if !ok {
					alias := channelAlias(cfg.OAuthModelAlias[channel], candidate.ID)
					exposed := []string{candidate.ID}
					if alias != nil {
						exposed = []string{alias.Name}
						if alias.KeepOriginal {
							exposed = append(exposed, candidate.ID)
						}
					}
					prefixed := make([]string, len(exposed))
					for i, id := range exposed {
						prefixed[i] = withPrefix(prefix, id)
					}
					kind := "chat"
					if candidate.Image {
						kind = "image"
					}
					model := &oauthSettingsModel{
						Channel: channel, Source: sourceAccount, Upstream: candidate.ID,
						ID: prefixed[0], Name: exposed[0], Exposed: prefixed, Alias: alias,
						DisplayName: candidate.DisplayName, Kind: kind,
						Supports: modelSupports{MaxOutputTokens: true, KeepOriginal: true},
					}
					if setting := config.ResolveOAuthModelSetting(settings[channel], exposed[0], candidate.ID, ""); setting != nil {
						copied := *setting
						copied.ThinkingLevels = append([]string(nil), setting.ThinkingLevels...)
						model.Setting = &copied
					}
					state = &rowState{model: model}
					rows[key] = state
					order = append(order, key)
				}
				state.model.Accounts++
				if pattern := matchingPattern(global, candidate.ID); pattern != "" {
					state.channelPattern = pattern
					continue
				}
				if pattern := matchingPattern(perAccount, candidate.ID); pattern != "" {
					state.accountExcluded++
					state.accountPattern = pattern
					continue
				}
				state.enabledAccounts++
			}
		}
	}
	for _, key := range order {
		state := rows[key]
		model := state.model
		model.Enabled = state.enabledAccounts > 0
		if !model.Enabled {
			if state.channelPattern != "" {
				model.DisabledBy = &modelDisabledBy{Scope: "channel", Pattern: patternIfWildcard(state.channelPattern, model.Upstream)}
			} else {
				model.DisabledBy = &modelDisabledBy{Scope: "account", Pattern: patternIfWildcard(state.accountPattern, model.Upstream), Accounts: state.accountExcluded}
			}
		}
		cat.models = append(cat.models, *model)
	}

	for i := range cfg.OpenAICompatibility {
		compat := cfg.OpenAICompatibility[i]
		name := strings.TrimSpace(compat.Name)
		if compat.Disabled || name == "" {
			continue
		}
		channel := compatChannelPrefix + name
		addChannel(channel, name, sourceAPIKey)
		prefix := strings.Trim(strings.TrimSpace(compat.Prefix), "/")
		for _, m := range compat.Models {
			upstream := strings.TrimSpace(m.Name)
			if upstream == "" {
				continue
			}
			exposed := upstream
			var alias *modelAliasView
			if a := strings.TrimSpace(m.Alias); a != "" && !strings.EqualFold(a, upstream) {
				exposed = a
				alias = &modelAliasView{Name: a}
			}
			kind := "chat"
			if m.Image {
				kind = "image"
			}
			model := oauthSettingsModel{
				Channel: channel, Source: sourceAPIKey, Upstream: upstream,
				ID: withPrefix(prefix, exposed), Name: exposed, Exposed: []string{withPrefix(prefix, exposed)},
				Alias: alias, Kind: kind, DisplayName: strings.TrimSpace(m.DisplayName), Enabled: true,
			}
			if pattern := compatExcludedPattern(m, compat.ExcludedModels); pattern != "" {
				model.Enabled = false
				model.DisabledBy = &modelDisabledBy{Scope: "provider", Pattern: patternIfWildcard(pattern, upstream)}
			}
			setting := config.OAuthModelSetting{Name: upstream, DisplayName: strings.TrimSpace(m.DisplayName), MaxContextLength: m.MaxContextLength}
			if m.Thinking != nil && len(m.Thinking.Levels) > 0 {
				setting.ThinkingLevels = append([]string(nil), m.Thinking.Levels...)
			}
			if setting.HasOverrides() {
				model.Setting = &setting
			}
			cat.models = append(cat.models, model)
		}
	}

	for i := range cat.models {
		model := &cat.models[i]
		if detail, ok := details[model.ID]; ok && model.Enabled {
			model.DisplayName = detail.DisplayName
			model.Kind = detail.Kind
			model.ContextLength = detail.ContextLength
			model.InputLength = detail.InputLength
			model.MaxCompletionTokens = detail.MaxCompletionTokens
			model.Reasoning = detail.Reasoning
		}
	}
	sort.SliceStable(cat.models, func(i, j int) bool {
		if cat.models[i].Channel != cat.models[j].Channel {
			return cat.models[i].Channel < cat.models[j].Channel
		}
		return strings.ToLower(cat.models[i].Upstream) < strings.ToLower(cat.models[j].Upstream)
	})
	sort.SliceStable(cat.channels, func(i, j int) bool {
		if cat.channels[i].Source != cat.channels[j].Source {
			return cat.channels[i].Source == sourceAccount
		}
		return strings.ToLower(cat.channels[i].Label) < strings.ToLower(cat.channels[j].Label)
	})
	return cat
}

// GetOAuthSettingsModels lists the model catalogue with current limits, enabled state,
// aliases, settings and the thinking level names CPA accepts.
func (h *Handler) GetOAuthSettingsModels(c *gin.Context) {
	h.mu.Lock()
	cfg := h.cfg
	manager := h.authManager
	detailsProvider := h.modelDetails
	settings := cloneOAuthSettings(cfg.OAuthSettings)
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
	h.mu.Lock()
	cat := buildModelCatalogue(h.cfg, manager, details)
	h.mu.Unlock()

	channelIDs := make([]string, 0, len(cat.channels))
	for _, ch := range cat.channels {
		channelIDs = append(channelIDs, ch.ID)
	}
	for channel := range settings {
		found := false
		for _, id := range channelIDs {
			if id == channel {
				found = true
				break
			}
		}
		if !found {
			channelIDs = append(channelIDs, channel)
			cat.channels = append(cat.channels, catalogueChannel{ID: channel, Label: channel, Source: sourceAccount})
		}
	}
	if settings == nil {
		settings = map[string][]config.OAuthModelSetting{}
	}
	models := cat.models
	if models == nil {
		models = []oauthSettingsModel{}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"channels":        channelIDs,
		"channel-info":    cat.channels,
		"models":          models,
		"oauth-settings":  settings,
		"thinking-levels": append([]string(nil), config.OAuthSettingThinkingLevels...),
		"details-hash":    hash,
	})
}

type modelConfigPatch struct {
	Channel string          `json:"channel"`
	Model   string          `json:"model"`
	Enabled *bool           `json:"enabled"`
	Alias   json.RawMessage `json:"alias"`
	Setting json.RawMessage `json:"setting"`
}

type modelConfigError struct {
	status int
	body   gin.H
}

func (e *modelConfigError) Error() string { return fmt.Sprint(e.body["error"]) }

func badModelConfig(status int, msg string, extra ...any) *modelConfigError {
	body := gin.H{"error": msg}
	for i := 0; i+1 < len(extra); i += 2 {
		body[fmt.Sprint(extra[i])] = extra[i+1]
	}
	return &modelConfigError{status: status, body: body}
}

// PatchModelConfig changes one model's enabled state, alias and/or settings:
//
//	{"channel": "codex" | "compat:<provider>", "model": "<upstream name>",
//	 "enabled": true|false, "alias": {"name": "...", "keep-original": false} | null,
//	 "setting": {"display-name": ..., "max-context-length": ..., ...} | null}
//
// Absent fields stay unchanged; alias/setting null (or an empty name / no overrides) removes
// them. Enabling a model also removes it from the credentials' own excluded_models.
func (h *Handler) PatchModelConfig(c *gin.Context) {
	var body modelConfigPatch
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	body.Channel = strings.TrimSpace(body.Channel)
	body.Model = strings.TrimSpace(body.Model)
	if body.Channel == "" || body.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel and model are required"})
		return
	}
	if body.Enabled == nil && body.Alias == nil && body.Setting == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to change"})
		return
	}
	alias, errAlias := decodeAliasPatch(body.Alias)
	if errAlias != nil {
		c.JSON(errAlias.status, errAlias.body)
		return
	}
	setting, errSetting := decodeSettingPatch(body.Setting, body.Model)
	if errSetting != nil {
		c.JSON(errSetting.status, errSetting.body)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	cat := buildModelCatalogue(h.cfg, h.authManager, nil)
	var row *oauthSettingsModel
	for i := range cat.models {
		m := &cat.models[i]
		if m.Channel == body.Channel && strings.EqualFold(m.Upstream, body.Model) {
			row = m
			break
		}
	}
	if row == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found", "channel": body.Channel, "model": body.Model})
		return
	}
	if alias != nil && alias.Name != "" && !strings.EqualFold(alias.Name, row.Upstream) {
		if errCollide := checkAliasCollision(cat, row, alias.Name); errCollide != nil {
			c.JSON(errCollide.status, errCollide.body)
			return
		}
	}
	var errApply *modelConfigError
	if row.Source == sourceAPIKey {
		errApply = h.applyCompatModelConfigLocked(row, body.Enabled, alias, setting)
	} else {
		errApply = h.applyAccountModelConfigLocked(c, cat, row, body.Enabled, alias, setting)
	}
	if errApply != nil {
		c.JSON(errApply.status, errApply.body)
		return
	}
	h.persistLocked(c)
}

type aliasPatch struct {
	set          bool
	Name         string
	KeepOriginal bool
}

func decodeAliasPatch(raw json.RawMessage) (*aliasPatch, *modelConfigError) {
	if raw == nil {
		return nil, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return &aliasPatch{set: true}, nil
	}
	var v struct {
		Name         string `json:"name"`
		KeepOriginal bool   `json:"keep-original"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, badModelConfig(http.StatusBadRequest, "invalid alias")
	}
	v.Name = strings.TrimSpace(v.Name)
	if strings.ContainsAny(v.Name, ",\n\r\t") {
		return nil, badModelConfig(http.StatusBadRequest, "alias must not contain commas or line breaks")
	}
	return &aliasPatch{set: true, Name: v.Name, KeepOriginal: v.KeepOriginal && v.Name != ""}, nil
}

type settingPatch struct {
	clear   bool
	setting config.OAuthModelSetting
}

func decodeSettingPatch(raw json.RawMessage, upstream string) (*settingPatch, *modelConfigError) {
	if raw == nil {
		return nil, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return &settingPatch{clear: true}, nil
	}
	var entry config.OAuthModelSetting
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, badModelConfig(http.StatusBadRequest, "invalid setting")
	}
	entry.Name = upstream
	entry.Alias = ""
	clean, err := validateOAuthSetting(entry)
	if err != nil {
		return nil, badModelConfig(http.StatusBadRequest, err.Error())
	}
	if !clean.HasOverrides() {
		return &settingPatch{clear: true}, nil
	}
	return &settingPatch{setting: clean}, nil
}

// checkAliasCollision rejects an alias that another model already uses as its ID.
func checkAliasCollision(cat modelCatalogue, row *oauthSettingsModel, alias string) *modelConfigError {
	prefix := strings.TrimSuffix(row.ID, row.Name)
	wanted := strings.ToLower(prefix + alias)
	own := map[string]bool{}
	for _, id := range row.Exposed {
		own[strings.ToLower(id)] = true
	}
	for _, m := range cat.models {
		if m.Channel == row.Channel && strings.EqualFold(m.Upstream, row.Upstream) {
			continue
		}
		for _, id := range m.Exposed {
			if strings.ToLower(id) == wanted {
				return badModelConfig(http.StatusConflict, "another model already uses this name", "model", m.Upstream, "channel", m.Channel)
			}
		}
	}
	if !own[wanted] && len(registry.GetGlobalRegistry().GetModelProviders(prefix+alias)) > 0 {
		return badModelConfig(http.StatusConflict, "another model already uses this name")
	}
	return nil
}

func (h *Handler) applyAccountModelConfigLocked(c *gin.Context, cat modelCatalogue, row *oauthSettingsModel, enabled *bool, alias *aliasPatch, setting *settingPatch) *modelConfigError {
	channel := row.Channel
	upstream := row.Upstream
	auths := cat.channelAuths[channel]
	if alias != nil && alias.set && alias.KeepOriginal && alias.Name == "" {
		alias.KeepOriginal = false
	}
	excluded := config.NormalizeOAuthExcludedModels(h.cfg.OAuthExcludedModels)
	if excluded == nil {
		excluded = map[string][]string{}
	}
	providerKeys := map[string]bool{}
	for _, auth := range auths {
		providerKeys[strings.ToLower(strings.TrimSpace(auth.Provider))] = true
	}
	type authEdit struct {
		auth      *coreauth.Auth
		remaining []string
	}
	var edits []authEdit
	if enabled != nil {
		for pk := range providerKeys {
			list := excluded[pk]
			if !*enabled {
				if matchingPattern(list, upstream) == "" {
					excluded[pk] = append(list, upstream)
				}
				continue
			}
			kept := make([]string, 0, len(list))
			for _, item := range list {
				if !strings.EqualFold(strings.TrimSpace(item), upstream) {
					kept = append(kept, item)
				}
			}
			if pattern := matchingPattern(kept, upstream); pattern != "" {
				return badModelConfig(http.StatusConflict, "the model is disabled by a wildcard in oauth-excluded-models; edit it in the config", "pattern", pattern, "scope", "channel")
			}
			if len(kept) == 0 {
				delete(excluded, pk)
			} else {
				excluded[pk] = kept
			}
		}
		if *enabled {
			for _, auth := range auths {
				list := authExcludedModels(auth)
				if matchingPattern(list, upstream) == "" {
					continue
				}
				kept := make([]string, 0, len(list))
				for _, item := range list {
					if !strings.EqualFold(strings.TrimSpace(item), upstream) {
						kept = append(kept, item)
					}
				}
				if pattern := matchingPattern(kept, upstream); pattern != "" {
					return badModelConfig(http.StatusConflict, "the model is disabled by a wildcard in a credential's excluded_models", "pattern", pattern, "scope", "account")
				}
				if coreauth.IsPluginVirtualAuth(auth) {
					return badModelConfig(http.StatusConflict, "the model is disabled in a plugin-managed credential")
				}
				edits = append(edits, authEdit{auth: auth, remaining: kept})
			}
		}
	}

	aliases := sanitizedOAuthModelAlias(h.cfg.OAuthModelAlias)
	if aliases == nil {
		aliases = map[string][]config.OAuthModelAlias{}
	}
	settings := cloneOAuthSettings(h.cfg.OAuthSettings)
	if settings == nil {
		settings = map[string][]config.OAuthModelSetting{}
	}
	oldNames := map[string]bool{strings.ToLower(upstream): true}
	if row.Alias != nil {
		oldNames[strings.ToLower(row.Alias.Name)] = true
	}
	if alias != nil && alias.set {
		kept := make([]config.OAuthModelAlias, 0, len(aliases[channel])+1)
		for _, entry := range aliases[channel] {
			if !strings.EqualFold(strings.TrimSpace(entry.Name), upstream) {
				kept = append(kept, entry)
			}
		}
		if alias.Name != "" && !strings.EqualFold(alias.Name, upstream) {
			kept = append(kept, config.OAuthModelAlias{Name: upstream, Alias: alias.Name, Fork: alias.KeepOriginal})
		}
		if len(kept) == 0 {
			delete(aliases, channel)
		} else {
			aliases[channel] = kept
		}
		// Settings follow the model, not its old name: key them by the upstream name.
		for i := range settings[channel] {
			entry := &settings[channel][i]
			if entry.Alias == "" && oldNames[strings.ToLower(entry.Name)] {
				entry.Name = upstream
			}
		}
	}
	if setting != nil {
		if alias != nil && alias.Name != "" {
			oldNames[strings.ToLower(alias.Name)] = true
		}
		kept := make([]config.OAuthModelSetting, 0, len(settings[channel])+1)
		for _, entry := range settings[channel] {
			if entry.Alias == "" && oldNames[strings.ToLower(strings.TrimSpace(entry.Name))] {
				continue
			}
			kept = append(kept, entry)
		}
		if !setting.clear {
			kept = append(kept, setting.setting)
		}
		if len(kept) == 0 {
			delete(settings, channel)
		} else {
			settings[channel] = kept
		}
	}

	h.cfg.OAuthExcludedModels = config.NormalizeOAuthExcludedModels(excluded)
	h.cfg.OAuthModelAlias = sanitizedOAuthModelAlias(aliases)
	h.cfg.OAuthSettings = sanitizedOAuthSettings(settings)

	for _, edit := range edits {
		auth := edit.auth
		if auth.Metadata == nil {
			auth.Metadata = map[string]any{}
		}
		delete(auth.Metadata, "excluded-models")
		if len(edit.remaining) == 0 {
			delete(auth.Metadata, "excluded_models")
		} else {
			auth.Metadata["excluded_models"] = append([]string(nil), edit.remaining...)
		}
		synthesizer.ApplyAuthExcludedModelsMeta(auth, h.cfg, edit.remaining, "oauth")
		updated, err := h.authManager.Update(c.Request.Context(), auth)
		if err != nil {
			return badModelConfig(http.StatusInternalServerError, "failed to update credential: "+err.Error())
		}
		if h.postAuthPersistHook != nil {
			hookAuth := updated
			if hookAuth == nil {
				hookAuth = auth
			}
			if errHook := h.postAuthPersistHook(c.Request.Context(), hookAuth); errHook != nil {
				return badModelConfig(http.StatusInternalServerError, "post-auth persist hook failed: "+errHook.Error())
			}
		}
	}
	return nil
}

func (h *Handler) applyCompatModelConfigLocked(row *oauthSettingsModel, enabled *bool, alias *aliasPatch, setting *settingPatch) *modelConfigError {
	providerName := strings.TrimPrefix(row.Channel, compatChannelPrefix)
	ci := -1
	for i := range h.cfg.OpenAICompatibility {
		if strings.EqualFold(strings.TrimSpace(h.cfg.OpenAICompatibility[i].Name), providerName) {
			ci = i
			break
		}
	}
	if ci < 0 {
		return badModelConfig(http.StatusNotFound, "provider not found")
	}
	compat := &h.cfg.OpenAICompatibility[ci]
	mi := -1
	for i := range compat.Models {
		if strings.EqualFold(strings.TrimSpace(compat.Models[i].Name), row.Upstream) {
			mi = i
			break
		}
	}
	if mi < 0 {
		return badModelConfig(http.StatusNotFound, "model not found")
	}
	if alias != nil && alias.KeepOriginal {
		return badModelConfig(http.StatusBadRequest, "keeping the original name is not supported for API-key providers")
	}
	if setting != nil && !setting.clear && setting.setting.MaxOutputTokens > 0 {
		return badModelConfig(http.StatusBadRequest, "max reply is not configurable for API-key providers")
	}
	model := compat.Models[mi]
	excluded := append([]string(nil), compat.ExcludedModels...)
	oldAlias := strings.TrimSpace(model.Alias)
	if alias != nil && alias.set {
		if alias.Name == "" || strings.EqualFold(alias.Name, model.Name) {
			model.Alias = ""
		} else {
			model.Alias = alias.Name
		}
		// An exclusion written for the old alias keeps applying to the model.
		if oldAlias != "" && !strings.EqualFold(oldAlias, model.Alias) {
			for i, item := range excluded {
				if strings.EqualFold(strings.TrimSpace(item), oldAlias) {
					excluded[i] = model.Name
				}
			}
		}
	}
	if enabled != nil {
		if !*enabled {
			if compatExcludedPattern(model, excluded) == "" {
				excluded = append(excluded, model.Name)
			}
		} else {
			kept := make([]string, 0, len(excluded))
			for _, item := range excluded {
				t := strings.TrimSpace(item)
				if strings.EqualFold(t, model.Name) || (model.Alias != "" && strings.EqualFold(t, model.Alias)) || (oldAlias != "" && strings.EqualFold(t, oldAlias)) {
					continue
				}
				kept = append(kept, item)
			}
			if pattern := compatExcludedPattern(model, kept); pattern != "" {
				return badModelConfig(http.StatusConflict, "the model is disabled by a wildcard in the provider's excluded-models; edit it in the config", "pattern", pattern, "scope", "provider")
			}
			excluded = kept
		}
	}
	if setting != nil {
		if setting.clear {
			model.DisplayName = ""
			model.MaxContextLength = 0
			if model.Thinking != nil {
				thinking := *model.Thinking
				thinking.Levels = nil
				model.Thinking = &thinking
				if thinkingSupportEmpty(model.Thinking) {
					model.Thinking = nil
				}
			}
		} else {
			model.DisplayName = setting.setting.DisplayName
			model.MaxContextLength = setting.setting.MaxContextLength
			if len(setting.setting.ThinkingLevels) > 0 {
				thinking := registry.ThinkingSupport{}
				if model.Thinking != nil {
					thinking = *model.Thinking
				}
				thinking.Levels = append([]string(nil), setting.setting.ThinkingLevels...)
				model.Thinking = &thinking
			} else if model.Thinking != nil {
				thinking := *model.Thinking
				thinking.Levels = nil
				model.Thinking = &thinking
				if thinkingSupportEmpty(model.Thinking) {
					model.Thinking = nil
				}
			}
		}
	}
	compat.Models[mi] = model
	if len(excluded) == 0 {
		excluded = nil
	}
	compat.ExcludedModels = excluded
	return nil
}

func thinkingSupportEmpty(t *registry.ThinkingSupport) bool {
	if t == nil {
		return true
	}
	return len(t.Levels) == 0 && t.Min == 0 && t.Max == 0 && !t.ZeroAllowed && !t.DynamicAllowed
}

// channelAlias returns the configured alias of an upstream model on a channel.
func channelAlias(entries []config.OAuthModelAlias, upstream string) *modelAliasView {
	var out *modelAliasView
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		a := strings.TrimSpace(entry.Alias)
		if !strings.EqualFold(name, upstream) || a == "" || strings.EqualFold(a, name) {
			continue
		}
		if out == nil {
			out = &modelAliasView{Name: a}
		}
		if entry.Fork {
			out.KeepOriginal = true
		}
	}
	return out
}

func isCompatAuth(auth *coreauth.Auth) bool {
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	return auth.Attributes != nil && strings.TrimSpace(auth.Attributes["compat_name"]) != ""
}

// authExcludedModels reads a credential's own excluded_models (canonical) or excluded-models.
func authExcludedModels(auth *coreauth.Auth) []string {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	raw, ok := auth.Metadata["excluded_models"]
	if !ok {
		raw = auth.Metadata["excluded-models"]
	}
	var out []string
	switch v := raw.(type) {
	case []string:
		out = append(out, v...)
	case []any:
		for _, item := range v {
			if s, okString := item.(string); okString {
				out = append(out, s)
			}
		}
	}
	clean := out[:0]
	for _, item := range out {
		if t := strings.TrimSpace(item); t != "" {
			clean = append(clean, t)
		}
	}
	return clean
}

func withPrefix(prefix, id string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return id
	}
	return prefix + "/" + id
}

// matchingPattern returns the first exclusion entry matching the model (case-insensitive,
// "*" wildcards), or "".
func matchingPattern(patterns []string, model string) string {
	value := strings.ToLower(strings.TrimSpace(model))
	for _, item := range patterns {
		pattern := strings.ToLower(strings.TrimSpace(item))
		if pattern != "" && wildcardMatch(pattern, value) {
			return strings.TrimSpace(item)
		}
	}
	return ""
}

func compatExcludedPattern(model config.OpenAICompatibilityModel, excluded []string) string {
	if p := matchingPattern(excluded, model.Name); p != "" {
		return p
	}
	if a := strings.TrimSpace(model.Alias); a != "" {
		return matchingPattern(excluded, a)
	}
	return ""
}

func patternIfWildcard(pattern, model string) string {
	if strings.EqualFold(strings.TrimSpace(pattern), strings.TrimSpace(model)) {
		return ""
	}
	return pattern
}

// wildcardMatch mirrors the service's matchWildcard ("*" matches any run of characters).
func wildcardMatch(pattern, value string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(value, parts[0]) {
		return false
	}
	value = value[len(parts[0]):]
	last := parts[len(parts)-1]
	if !strings.HasSuffix(value, last) {
		return false
	}
	value = value[:len(value)-len(last)]
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		idx := strings.Index(value, part)
		if idx < 0 {
			return false
		}
		value = value[idx+len(part):]
	}
	return true
}
