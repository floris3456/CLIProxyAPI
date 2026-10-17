package auth

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func validateCachePolicyExecutor(provider ProviderExecutor, opts executor.Options) error {
	if opts.CachePolicy == nil {
		if cachepolicy.Managed(opts.Headers) {
			return cachepolicy.Unavailable()
		}
		return nil
	}
	if err := cachepolicy.Validate(*opts.CachePolicy); err != nil {
		return err
	}
	support, ok := provider.(interface{ SupportsCachePolicy() bool })
	if !ok || !support.SupportsCachePolicy() {
		return cachepolicy.Unsupported()
	}
	return nil
}
