package beads

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// NativeReadyFallbackReason checks stable gates shared with the store factory.
// Preflight and SDK opening failures remain retryable, not stable eligibility.
// The operator flag comes from the selected environment, not ambient process
// state that can temporarily belong to another scope's native opening.
func NativeReadyFallbackReason(scopeRoot, provider string, env map[string]string) string {
	value := strings.TrimSpace(env[nativeForceFallbackEnv])
	if value == "1" || strings.EqualFold(value, "true") {
		return nativeForceFallbackGate
	}
	if !contract.ProviderUsesBDContract(strings.TrimSpace(provider)) {
		return string(contract.PreflightCheckProviderContract)
	}
	if scopeHasExecutableBdHooks(scopeRoot) {
		return nativeHooksGate
	}
	return ""
}
