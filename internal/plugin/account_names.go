package plugin

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
)

const (
	maxAccountMetadataBytes = 4 << 20
	maxAccountNameBytes     = 1024
)

// hostAuthListResult intentionally decodes only the three metadata fields this
// projection needs. Credential JSON, paths, tokens and every other host field
// are neither returned nor retained.
type hostAuthListResult struct {
	OK     bool `json:"ok"`
	Result *struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
			Label     string `json:"label"`
		} `json:"files"`
	} `json:"result"`
}

// parseAccountLabels accepts only a successful host.auth.list envelope. A
// malformed, oversized or failed response yields no labels, so callers fall
// back to the unchanged AuthIndex instead of surfacing host errors.
func parseAccountLabels(raw []byte) map[string]string {
	if len(raw) == 0 || len(raw) > maxAccountMetadataBytes {
		return nil
	}
	var envelope hostAuthListResult
	if json.Unmarshal(raw, &envelope) != nil || !envelope.OK || envelope.Result == nil {
		return nil
	}
	labels := make(map[string]string, len(envelope.Result.Files))
	for _, file := range envelope.Result.Files {
		index := strings.TrimSpace(file.AuthIndex)
		if !validAuthIndex(index) {
			continue
		}
		name := accountDisplayName(file.Name, file.Label)
		if name == "" {
			continue
		}
		// The host lists entries deterministically; keep the first valid name
		// if a malformed host repeats an index.
		if _, exists := labels[index]; !exists {
			labels[index] = name
		}
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

func validAuthIndex(index string) bool {
	if index == "" || len(index) > maxAccountNameBytes {
		return false
	}
	for _, r := range index {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// accountDisplayName implements Name -> Label -> none. Each candidate is
// validated independently so an unusable file name may still fall back to a
// usable label; callers keep the original AuthIndex when both are unusable.
func accountDisplayName(name, label string) string {
	if validAccountName(name) {
		return strings.TrimSpace(name)
	}
	if validAccountName(label) {
		return strings.TrimSpace(label)
	}
	return ""
}

func validAccountName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxAccountNameBytes {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// accountLabelsLocked reads the installed lookup under the Manager lifecycle
// read lock. Callers must already hold m.mu for reading.
func (m *Manager) accountLabelsLocked(callbackID string) map[string]string {
	if callbackID == "" || m.accountMetadata == nil {
		return nil
	}
	return parseAccountLabels(m.accountMetadata(callbackID))
}

// projectAccountLabels changes only display labels for account groups. Group
// key, AuthIndex, aggregates, ordering and pagination remain engine-owned and
// are never rewritten from host metadata.
func projectAccountLabels(view *engine.RequestView, labels map[string]string) {
	if view == nil || len(labels) == 0 {
		return
	}
	for i := range view.Groups {
		if name := labels[view.Groups[i].AuthIndex]; name != "" {
			view.Groups[i].Label = name
		}
	}
}
