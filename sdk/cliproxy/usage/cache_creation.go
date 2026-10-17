package usage

import "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

func cloneCounter(v *int64) *int64 {
	if v == nil {
		return nil
	}
	n := *v
	return &n
}

func CloneCacheCreation(v *pluginapi.CacheCreationUsage) *pluginapi.CacheCreationUsage {
	if v == nil {
		return nil
	}
	out := *v
	out.TotalTokens = cloneCounter(v.TotalTokens)
	out.Ephemeral5mInputTokens = cloneCounter(v.Ephemeral5mInputTokens)
	out.Ephemeral1hInputTokens = cloneCounter(v.Ephemeral1hInputTokens)
	out.UnknownTokens = cloneCounter(v.UnknownTokens)
	return &out
}

// ClassifyCacheCreation never substitutes policy selection for reported counters.
func ClassifyCacheCreation(v *pluginapi.CacheCreationUsage) {
	if v == nil {
		return
	}
	v.UnknownTokens = nil
	invalid := v.Quality == "inconsistent"
	v.Quality = "unclassified"
	var known int64
	for _, counter := range []*int64{v.Ephemeral5mInputTokens, v.Ephemeral1hInputTokens} {
		if counter == nil {
			continue
		}
		if *counter < 0 || *counter > int64(^uint64(0)>>1)-known {
			invalid = true
			continue
		}
		known += *counter
	}
	if v.TotalTokens != nil && (*v.TotalTokens < 0 || known > *v.TotalTokens) {
		invalid = true
	}
	if invalid {
		v.Quality = "inconsistent"
		return
	}
	if v.TotalTokens == nil {
		return
	}
	u := *v.TotalTokens - known
	v.UnknownTokens = &u
	if u == 0 {
		v.Quality = "complete"
	} else {
		v.Quality = "partial"
	}
}

// MergeCacheCreation merges cumulative snapshots field by field, never adds them.
func MergeCacheCreation(old, update *pluginapi.CacheCreationUsage) *pluginapi.CacheCreationUsage {
	if old == nil {
		return CloneCacheCreation(update)
	}
	if update == nil {
		return CloneCacheCreation(old)
	}
	out := CloneCacheCreation(update)
	if out.TotalTokens == nil {
		out.TotalTokens = cloneCounter(old.TotalTokens)
	}
	if out.Ephemeral5mInputTokens == nil {
		out.Ephemeral5mInputTokens = cloneCounter(old.Ephemeral5mInputTokens)
	}
	if out.Ephemeral1hInputTokens == nil {
		out.Ephemeral1hInputTokens = cloneCounter(old.Ephemeral1hInputTokens)
	}
	ClassifyCacheCreation(out)
	return out
}
