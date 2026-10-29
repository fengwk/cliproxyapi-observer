//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

// These wire types deliberately stay local: the shared harness predates the
// additive key.secret field and must remain untouched by this key-material test.
type keyMaterialView struct {
	ID        string  `json:"id"`
	Secret    string  `json:"secret"`
	Enabled   bool    `json:"enabled"`
	Revoked   bool    `json:"revoked"`
	DeletedAt *string `json:"deleted_at"`
}

type keyMaterialList struct {
	Items []keyMaterialView `json:"items"`
}

type keyMaterialCreated struct {
	Key    keyMaterialView `json:"key"`
	Secret string          `json:"secret"`
}

func listKeyMaterials(t *testing.T, h *harness, path string) map[string]keyMaterialView {
	t.Helper()
	var out keyMaterialList
	h.api(t, http.MethodGet, path, nil, http.StatusOK, &out)
	items := make(map[string]keyMaterialView, len(out.Items))
	for _, item := range out.Items {
		items[item.ID] = item
	}
	return items
}

// The user explicitly authorized permanent plaintext client Key management.
// This fresh-database real-host test proves generated, custom and rotated
// material survives restarts and remains in authenticated lists, while disable
// retains it, revoke/delete clear it, duplicate creates stay atomic and nothing
// reaches usage, facets, summaries, health or logs.
func TestClientKeyPlaintextMaterialPersistsThroughManagement(t *testing.T) {
	h := newHarness(t)
	generated := h.createKey(t, validSpec())
	if current := listKeyMaterials(t, h, "/keys")[generated.Key.ID]; current.Secret != generated.Secret {
		t.Fatalf("generated plaintext missing from list: %+v", current)
	}

	customSecret := "native-custom-key-material-123456!"
	input := struct {
		keySpec
		Secret string `json:"secret"`
	}{keySpec: validSpec(), Secret: customSecret}
	var created keyMaterialCreated
	h.api(t, http.MethodPost, "/keys", input, http.StatusCreated, &created)
	if created.Secret != customSecret || created.Key.Secret != customSecret || created.Key.ID == generated.Key.ID {
		t.Fatalf("custom material response: %+v", created)
	}
	customID := created.Key.ID
	beforeDuplicate := listKeyMaterials(t, h, "/keys")
	h.api(t, http.MethodPost, "/keys", input, http.StatusConflict, nil)
	if afterDuplicate := listKeyMaterials(t, h, "/keys"); !reflect.DeepEqual(beforeDuplicate, afterDuplicate) {
		t.Fatal("duplicate rejection changed key material or state")
	}

	h.awaitGracefulRestartReady(t, 0)
	restarted, err := h.restartPreservingConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	h = restarted
	afterRestart := listKeyMaterials(t, h, "/keys")
	if afterRestart[generated.Key.ID].Secret != generated.Secret || afterRestart[customID].Secret != customSecret {
		t.Fatalf("plaintext did not survive restart: %+v", afterRestart)
	}

	next := "native-rotated-key-material-654321!"
	query := "?id=" + url.QueryEscape(customID)
	var rotated keyMaterialCreated
	h.api(t, http.MethodPost, "/keys/rotate"+query, map[string]any{"secret": next}, http.StatusOK, &rotated)
	if rotated.Secret != next || rotated.Key.Secret != next || rotated.Key.ID != customID {
		t.Fatalf("rotation response: %+v", rotated)
	}
	if current := listKeyMaterials(t, h, "/keys")[customID]; current.Secret != next {
		t.Fatalf("rotated plaintext missing from list: %+v", current)
	}
	denyInference(t, h, customSecret)
	if status, _ := h.infer(t, next, false); status != http.StatusOK {
		t.Fatalf("rotated plaintext rejected: %d", status)
	}
	h.await(t, "rotated key execution accounted", func() bool { return len(h.page(t, "page_size=100").Items) == 1 })
	records := h.page(t, "page_size=100")

	spec := validSpec()
	spec.Enabled = false
	var disabled keyMaterialView
	h.api(t, http.MethodPut, "/keys?id="+url.QueryEscape(generated.Key.ID), spec, http.StatusOK, &disabled)
	if disabled.Secret != generated.Secret || disabled.Enabled {
		t.Fatalf("disable lost or changed plaintext: %+v", disabled)
	}
	denyInference(t, h, generated.Secret)

	h.awaitGracefulRestartReady(t, 1)
	restarted, err = h.restartPreservingConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	h = restarted
	afterDisableRestart := listKeyMaterials(t, h, "/keys")
	if current := afterDisableRestart[generated.Key.ID]; current.Secret != generated.Secret || current.Enabled {
		t.Fatalf("disabled plaintext did not survive restart: %+v", current)
	}
	if current := afterDisableRestart[customID]; current.Secret != next {
		t.Fatalf("rotated plaintext did not survive restart: %+v", current)
	}

	h.api(t, http.MethodDelete, "/keys?id="+url.QueryEscape(customID), nil, http.StatusNoContent, nil)
	revoked := listKeyMaterials(t, h, "/keys")[customID]
	if revoked.Secret != "" || !revoked.Revoked || revoked.DeletedAt != nil {
		t.Fatalf("revocation retained plaintext or lost state: %+v", revoked)
	}
	denyInference(t, h, next)

	h.api(t, http.MethodPost, "/keys/delete?id="+url.QueryEscape(generated.Key.ID), nil, http.StatusNoContent, nil)
	all := listKeyMaterials(t, h, "/keys?include_deleted=true")
	if deleted := all[generated.Key.ID]; deleted.Secret != "" || deleted.DeletedAt == nil {
		t.Fatalf("delete retained plaintext or lost tombstone: %+v", deleted)
	}
	if deleted := all[customID]; deleted.Secret != "" || deleted.DeletedAt != nil {
		t.Fatalf("revoked key regained plaintext: %+v", deleted)
	}
	if after := h.page(t, "page_size=100"); !reflect.DeepEqual(records, after) {
		t.Fatal("key management changed accounting records")
	}

	for _, path := range []string{"/requests?page_size=100", "/summary", "/health", "/requests/facets?field=model"} {
		raw := h.api(t, http.MethodGet, path, nil, http.StatusOK, nil)
		if bytes.Contains(raw, []byte(generated.Secret)) || bytes.Contains(raw, []byte(next)) || bytes.Contains(raw, []byte(customSecret)) {
			t.Fatalf("%s exposed client key plaintext", path)
		}
	}
}
