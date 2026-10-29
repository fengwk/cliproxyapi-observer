//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Two fake Observer Keys send the same existing mock model through a real CPA
// host. The composite view must keep the two Key IDs separate while returning
// the original model/provider identity and the configured Key labels.
func TestKeyModelNativeRouteIdentity(t *testing.T) {
	h := newHarness(t)
	first := h.createKey(t, keySpec{Label: "Fixture Key A", Enabled: true, AllowedModels: []string{}})
	second := h.createKey(t, keySpec{Label: "Fixture Key B", Enabled: true, AllowedModels: []string{}})

	for _, created := range []createdKey{first, second} {
		status, body, err := h.rawRequest(http.MethodPost, "/v1/chat/completions", chatBody(false),
			map[string]string{"Authorization": "Bearer " + created.Secret})
		if err != nil || status != http.StatusOK {
			t.Fatalf("inference status=%d err=%v body=%s", status, err, truncate(body, 500))
		}
	}

	var records requestPage
	h.await(t, "both composite usage records", func() bool {
		records = h.page(t, "page_size=100")
		return len(records.Items) == 2
	})
	identities := map[string][2]string{}
	for _, record := range records.Items {
		identities[record.KeyID] = [2]string{record.Provider, record.Model}
	}
	if len(identities) != 2 || identities[first.Key.ID][0] == "" || identities[second.Key.ID][0] == "" {
		t.Fatalf("record identities = %+v", identities)
	}

	date := time.Now().UTC().Format("2006-01-02")
	var view struct {
		Groups []struct {
			Key      string `json:"key"`
			Label    string `json:"label"`
			KeyID    string `json:"key_id"`
			Model    string `json:"model"`
			Provider string `json:"provider"`
			Requests int64  `json:"requests"`
		} `json:"groups"`
	}
	h.api(t, http.MethodGet, "/requests/view?from_date="+date+"&to_date="+date+"&view=key_model&page_size=100",
		nil, http.StatusOK, &view)
	if len(view.Groups) != 2 {
		t.Fatalf("key_model groups = %+v", view.Groups)
	}
	want := map[string]string{first.Key.ID: first.Key.Label, second.Key.ID: second.Key.Label}
	seen := map[string]bool{}
	for _, group := range view.Groups {
		label, ok := want[group.KeyID]
		if !ok || seen[group.KeyID] {
			t.Fatalf("unexpected/mixed group = %+v", group)
		}
		seen[group.KeyID] = true
		recordIdentity := identities[group.KeyID]
		if group.Label != label || group.Model != recordIdentity[1] || group.Provider != recordIdentity[0] || group.Requests != 1 {
			t.Fatalf("group identity = %+v, want label %q model %q provider %q", group, label, recordIdentity[1], recordIdentity[0])
		}
		var identity []string
		if json.Unmarshal([]byte(group.Key), &identity) != nil || len(identity) != 3 ||
			identity[0] != group.KeyID || identity[1] != group.Provider || identity[2] != group.Model {
			t.Fatalf("group key is not [key_id,provider,model]: %q", group.Key)
		}
	}
}
