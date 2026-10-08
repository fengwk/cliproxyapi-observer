//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Two real file-backed credentials with the same label remain distinct in usage
// and all aggregate views. Kimi's explicit base_url keeps inference on loopback.
func TestAuthFileDimensionFilteringAndStats(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if key := r.Header.Get("Authorization"); key != "Bearer fake-file-token-a" && key != "Bearer fake-file-token-b" {
			t.Error("file credential not used by local upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"file-fixture","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":%s}`, fixtureText, usageFixtureJSON())
	}))
	defer local.Close()
	h := newHarness(t)
	names := []string{"fake-file-a.json", "fake-file-b.json"}
	for i, name := range names {
		letter := string(rune('a' + i))
		raw := []byte(fmt.Sprintf(`{"type":"kimi","prefix":"file-%s","email":"same-label","access_token":"fake-file-token-%s","base_url":%q}`, letter, letter, local.URL+"/v1"))
		status, _, _, err := h.rawRequestWithHeaders(http.MethodPost, "/v8/management/credentials?name="+name, raw,
			map[string]string{"Authorization": "Bearer " + mgmtKey, "Content-Type": "application/json"})
		if err != nil || status != 200 {
			t.Fatalf("fake file upload: status %d err %v", status, err)
		}
	}
	indexes := make(map[string]string)
	models := make(map[string]string)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, _, raw := h.managementRequest(t, http.MethodGet, "/v8/management/credentials")
		var listing struct {
			Files []struct {
				Name  string `json:"name"`
				Label string `json:"label"`
				Index string `json:"auth_index"`
			} `json:"files"`
		}
		if status != 200 || json.Unmarshal(raw, &listing) != nil {
			t.Fatal("file listing unavailable")
		}
		for _, entry := range listing.Files {
			for _, name := range names {
				if entry.Name == name {
					if entry.Label != "same-label" || len(entry.Index) != 16 {
						t.Fatal("file listing lost label/index")
					}
					indexes[name] = entry.Index
				}
			}
		}
		_, raw = h.clientRequest(t, http.MethodGet, "/v1/models", nil)
		var catalog struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &catalog) != nil {
			t.Fatal("file models unavailable")
		}
		for _, entry := range catalog.Data {
			for _, prefix := range []string{"file-a/", "file-b/"} {
				if strings.HasPrefix(entry.ID, prefix) {
					models[prefix] = entry.ID
				}
			}
		}
		if len(indexes) == 2 && len(models) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(indexes) != 2 || len(models) != 2 || indexes[names[0]] == indexes[names[1]] {
		t.Fatal("distinct file indexes/models not registered")
	}
	for i, name := range names {
		index := indexes[name]
		model := models[fmt.Sprintf("file-%c/", 'a'+i)]
		deadline = time.Now().Add(10 * time.Second)
		observed := false
		for time.Now().Before(deadline) {
			payload := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"fake file check"}],"stream":false}`, model))
			status, raw := h.clientRequest(t, http.MethodPost, "/v1/chat/completions", payload)
			if status != 200 || !strings.Contains(string(raw), fixtureText) {
				t.Fatalf("local file inference status %d", status)
			}
			time.Sleep(150 * time.Millisecond)
			if len(readIdentityPage(t, h, "auth_index="+index).Items) > 0 {
				observed = true
				break
			}
		}
		if !observed {
			t.Fatal("file usage not attributed")
		}
		page := readIdentityPage(t, h, "auth_index="+index)
		for _, item := range page.Items {
			if item.AuthIndex != index {
				t.Fatal("file request filter crossed credentials")
			}
		}
		status, _, raw := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/summary?auth_index="+index)
		var summary struct {
			Totals struct {
				Requests    uint64 `json:"requests"`
				CacheHits   uint64 `json:"cache_hits"`
				InputTokens uint64 `json:"input_tokens"`
			} `json:"totals"`
			Credentials []struct {
				ID       string `json:"id"`
				Requests uint64 `json:"requests"`
			} `json:"credentials"`
			Series []struct {
				Requests uint64 `json:"requests"`
			} `json:"series"`
		}
		if status != 200 || json.Unmarshal(raw, &summary) != nil || summary.Totals.Requests == 0 ||
			summary.Totals.InputTokens != 10*summary.Totals.Requests || summary.Totals.CacheHits != summary.Totals.Requests ||
			len(summary.Credentials) != 1 || summary.Credentials[0].ID != index ||
			summary.Credentials[0].Requests != summary.Totals.Requests {
			t.Fatal("file group stats disagree")
		}
		var series uint64
		for _, p := range summary.Series {
			series += p.Requests
		}
		if series != summary.Totals.Requests {
			t.Fatal("file trend filter disagrees")
		}
	}
	// Deleting a credential must not erase its retained usage dimension.
	status, _, _ := h.managementRequest(t, http.MethodDelete, "/v8/management/credentials?name="+names[0])
	if status != 200 {
		t.Fatal("fake file deletion failed")
	}
	status, _, raw := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/summary?auth_index="+indexes[names[0]])
	var after summaryView
	if status != 200 || json.Unmarshal(raw, &after) != nil || after.Totals.Requests == 0 {
		t.Fatal("deletion erased historical file stats")
	}
	h.Stop()
	stored, err := os.ReadFile(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"fake-file-token-a", "fake-file-token-b", h.authDir} {
		if bytes.Contains(stored, []byte(private)) {
			t.Fatal("observer persisted file credentials or path")
		}
	}
}
