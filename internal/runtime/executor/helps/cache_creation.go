package helps

import (
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func parseClaudeCacheCreation(node gjson.Result) *pluginapi.CacheCreationUsage {
	out := &pluginapi.CacheCreationUsage{}
	if split := node.Get("cache_creation"); split.Exists() && !split.IsObject() {
		out.Quality = "inconsistent"
	}
	read := func(path string) *int64 {
		v := node.Get(path)
		if !v.Exists() {
			return nil
		}
		n, err := strconv.ParseInt(v.Raw, 10, 64)
		if err != nil || v.Type != gjson.Number {
			out.Quality = "inconsistent"
			return nil
		}
		return &n
	}
	out.TotalTokens = read("cache_creation_input_tokens")
	out.Ephemeral5mInputTokens = read("cache_creation.ephemeral_5m_input_tokens")
	out.Ephemeral1hInputTokens = read("cache_creation.ephemeral_1h_input_tokens")
	if out.TotalTokens == nil && out.Ephemeral5mInputTokens == nil && out.Ephemeral1hInputTokens == nil && out.Quality == "" {
		return nil
	}
	usage.ClassifyCacheCreation(out)
	return out
}
