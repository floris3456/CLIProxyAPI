package pluginhost

import (
	"context"
	"sort"
	"strings"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Additive provenance for host.auth.list. Files retain their existing visibility;
// neither a disk catalog nor a successfully loaded runtime is coverage evidence.
type authCatalogDescriptor struct {
	Version       int    `json:"version"`
	Domain        string `json:"domain"`
	State         string `json:"state"`
	Generation    uint64 `json:"generation"`
	ListenerState string `json:"listener_state"`
}

func (h *Host) listAuthCatalog(ctx context.Context) ([]pluginapi.HostAuthFileEntry, authCatalogDescriptor, error) {
	d := authCatalogDescriptor{Version: 1, Domain: "cpa.auth.disk", State: "uninitialized", ListenerState: h.usageListenerState(ctx)}
	manager := h.currentAuthManager()
	if manager == nil {
		files, err := h.listAuthFilesFromDisk()
		return files, d, err
	}
	auths, state, generation := manager.CatalogSnapshot()
	d.Domain, d.State, d.Generation = "cpa.auth.runtime", state, generation
	if manager.HomeEnabled() {
		d.State = "unsupported"
	}
	files := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
	for _, a := range auths {
		if entry := h.buildHostAuthFileEntry(a); entry != nil {
			files = append(files, *entry)
		}
	}
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name) })
	return files, d, nil
}

// Registration runs before publication of capabilities and installation of the
// usage adapter. Distinguish that real lifecycle from a missing/retired listener.
func (h *Host) usageListenerState(ctx context.Context) string {
	id := hostCallbackPluginIDFromContext(ctx)
	if id == "" {
		return "unavailable"
	}
	instance := hostCallbackInstanceFromContext(ctx)
	h.mu.Lock()
	loading := h.loading[id]
	registering := false
	if loading != nil && !loading.closed {
		_, registering = loading.callbackInstances[instance]
	}
	h.mu.Unlock()
	if registering {
		return "registering"
	}
	current := h.currentUsagePlugin(id)
	if current == nil || h.isPluginFused(id) {
		return "unavailable"
	}
	if instance != nil {
		rpc, ok := current.(*rpcPluginAdapter)
		if !ok || pluginCallbackInstance(rpc.client) != instance {
			return "retired"
		}
	}
	registered := coreusage.DefaultManager().NamedPlugin("plugin:" + id)
	adapter, ok := registered.(*usageAdapter)
	if !ok || adapter.host != h || !sameUsagePlugin(adapter.plugin, current) {
		return "installing"
	}
	return "ready"
}
