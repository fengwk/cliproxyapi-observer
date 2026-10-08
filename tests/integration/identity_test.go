//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type identityPage struct {
	Items []struct {
		ClientKeyID string `json:"client_key_id"`
		AuthIndex   string `json:"auth_index"`
	} `json:"items"`
	HasMore bool `json:"has_more"`
}

func readIdentityPage(t *testing.T, h *harness, query string) identityPage {
	t.Helper()
	status, _, raw := h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/requests?"+query)
	if status != http.StatusOK {
		t.Fatalf("identity requests status %d", status)
	}
	for _, secret := range []string{clientKey, clientKeyTwo, upstreamKey} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("request metadata leaked a credential")
		}
	}
	var page identityPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

// The native host forwards the downstream principal and selected upstream index
// independently. Filtering selects statistics without changing inference.
func TestRequestKeyIdentitiesAndRestart(t *testing.T) {
	h := newHarness(t)
	h.sendAndObserve(t, 10*time.Second, func(page requestPage) bool { return len(page.Items) > 0 })
	first := readIdentityPage(t, h, "limit=1").Items[0]
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(first.ClientKeyID) ||
		!regexp.MustCompile(`^[a-f0-9]{16}$`).MatchString(first.AuthIndex) {
		t.Fatal("host identity metadata missing")
	}
	status, _, _, err := h.rawRequestWithHeaders(http.MethodPost, "/v1/chat/completions", chatBody(false),
		map[string]string{"Authorization": "Bearer " + clientKeyTwo})
	if err != nil || status != http.StatusOK {
		t.Fatalf("second client inference: status %d err %v", status, err)
	}
	var second string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, item := range readIdentityPage(t, h, "limit=50").Items {
			if item.ClientKeyID != first.ClientKeyID {
				second = item.ClientKeyID
			}
		}
		if second != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if second == "" {
		t.Fatal("distinct downstream keys did not produce distinct fingerprints")
	}
	for _, fingerprint := range []string{first.ClientKeyID, second} {
		page := readIdentityPage(t, h, "client_key_id="+fingerprint+"&auth_index="+first.AuthIndex)
		if len(page.Items) == 0 {
			t.Fatal("combined identity filter lost matching records")
		}
		for _, item := range page.Items {
			if item.ClientKeyID != fingerprint || item.AuthIndex != first.AuthIndex {
				t.Fatal("identity filter returned a nonmatching record")
			}
		}
	}
	if len(readIdentityPage(t, h, "client_key_id=unknown").Items) != 0 {
		t.Fatal("attributed request entered unknown group")
	}
	status, _, raw := h.managementRequest(t, http.MethodGet, "/v8/management/credentials")
	if status != http.StatusOK {
		t.Fatal("CPA credential name route not available")
	}
	var names struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
			Label     string `json:"label"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &names); err != nil {
		t.Fatal("credential listing is malformed")
	}
	// Some host versions omit runtime-only config credentials from this listing.
	// Real file-backed name/index correlation is asserted in auth_files_test.go.
	for _, entry := range names.Files {
		if entry.AuthIndex == first.AuthIndex && entry.Name == "" && entry.Label == "" {
			t.Fatal("listed fixture credential has no display name")
		}
	}
	status, _, _ = h.managementRequest(t, http.MethodGet, "/v0/management/plugins/"+pluginID+"/summary?client_key_id="+first.ClientKeyID)
	if status != http.StatusOK {
		t.Fatal("summary key filtering failed")
	}
	h.Stop()
	db, err := os.ReadFile(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{clientKey, clientKeyTwo, upstreamKey} {
		if bytes.Contains(db, []byte(secret)) {
			t.Fatal("database contains plaintext credential")
		}
	}
	restarted, err := startHost(t, h.mock, h.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Stop)
	restarted.sendAndObserve(t, 10*time.Second, func(page requestPage) bool { return len(page.Items) > 2 })
	for _, item := range readIdentityPage(t, restarted, "limit=50").Items {
		if item.ClientKeyID != first.ClientKeyID && item.ClientKeyID != second {
			t.Fatal("identity changed after host restart")
		}
	}
	if len(readIdentityPage(t, restarted, "client_key_id="+strings.Repeat("0", 64)).Items) != 0 {
		t.Fatal("unknown fingerprint unexpectedly matched")
	}
	h.mock.requireClean(t)
}
