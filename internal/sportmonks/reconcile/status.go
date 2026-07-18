package reconcile

import (
	"strings"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/modules/matches"
)

// NormalizeProviderStatus maps a Sportmonks fixture status label to a local match status.
func NormalizeProviderStatus(status string) string {
	return normalizeStatus(status)
}

// IsExplicitTerminalProviderStatus reports provider phases that definitively end a fixture.
func IsExplicitTerminalProviderStatus(status string) bool {
	lower := strings.ToLower(strings.TrimSpace(status))
	return strings.Contains(lower, "finished") ||
		strings.Contains(lower, "aban") ||
		strings.Contains(lower, "cancl")
}

// IsTerminalProviderStatus reports whether the provider phase should close the public match.
func IsTerminalProviderStatus(status string) bool {
	local := NormalizeProviderStatus(status)
	return local == matches.StatusCompleted || local == matches.StatusAbandoned
}

// FixtureProviderStatus reads the provider status field from a fixture JSON payload.
func FixtureProviderStatus(raw []byte) (string, bool) {
	value, err := decodeJSON(raw)
	if err != nil {
		return "", false
	}
	root, err := unwrapObject(value)
	if err != nil {
		return "", false
	}
	status, ok := stringField(root, "status")
	return status, ok && strings.TrimSpace(status) != ""
}
