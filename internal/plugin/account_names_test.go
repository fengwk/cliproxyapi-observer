package plugin

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fengwk/cliproxyapi-observer/internal/engine"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func dispatchCallbackForTest(t *testing.T, m *Manager, method, path string, query url.Values, callbackID string) pluginapi.ManagementResponse {
	t.Helper()
	return m.dispatchManagement(managementRequest{
		ManagementRequest: pluginapi.ManagementRequest{Method: method, Path: path, Query: query},
		HostCallbackID:    callbackID,
	})
}

func accountMetadataPayload(t *testing.T, files ...map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"files": files}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The management projection consumes only the host's read-only metadata list,
// applies it to response records/account groups/auth_index facets, and leaves aggregates,
// record history, identities and pagination untouched.
func TestAccountMetadataProjectionAndFacetSearch(t *testing.T) {
	m := NewManager(nil)
	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	call(t, m, "plugin.register", lifecycleRequest{[]byte("db: " + filepath.Join(t.TempDir(), "observer.db")), 6})
	t.Cleanup(func() { call(t, m, "plugin.shutdown", nil) })
	key, _, err := m.store.CreateKey(engine.KeySpec{Enabled: true, Label: "Client"})
	if err != nil {
		t.Fatal(err)
	}
	at := m.now().UTC()
	for _, index := range []string{"auth-a", "auth-b"} {
		if err := m.store.RecordUsage(pluginapi.UsageRecord{
			RequestID: "request-" + index, APIKey: "observer:" + key.ID, Generate: true,
			RequestedAt: at, Model: "exact/model", Provider: "openai", AuthIndex: index,
			Detail: pluginapi.UsageDetail{InputTokens: 8, OutputTokens: 2, TotalTokens: 10},
		}); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Second)
	date := at.Format("2006-01-02")
	baseQuery := url.Values{"from_date": {date}, "to_date": {date}, "view": {"account"}}

	var baseline engine.RequestView
	response := dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", baseQuery, "")
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &baseline) != nil {
		t.Fatalf("baseline account view: %d %s", response.StatusCode, response.Body)
	}
	if len(baseline.Groups) != 2 {
		t.Fatalf("baseline groups = %+v", baseline.Groups)
	}
	for _, group := range baseline.Groups {
		if group.Label != group.AuthIndex {
			t.Fatalf("baseline label did not fall back to index: %+v", group)
		}
	}

	calls := 0
	callbackIDs := []string{}
	m.SetAccountMetadataLookup(func(callbackID string) []byte {
		calls++
		callbackIDs = append(callbackIDs, callbackID)
		return accountMetadataPayload(t,
			map[string]any{"auth_index": "auth-a", "name": "Alpha File", "label": "Alpha Label", "path": "/not-stored", "token": "host-secret-not-returned"},
			map[string]any{"auth_index": "auth-b", "name": "", "label": "Beta Label"},
			map[string]any{"auth_index": "auth-missing", "name": "Unused Account"},
		)
	})

	var account engine.RequestView
	response = dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", baseQuery, "callback-1")
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &account) != nil {
		t.Fatalf("account view: %d %s", response.StatusCode, response.Body)
	}
	if calls != 1 || len(callbackIDs) != 1 || callbackIDs[0] != "callback-1" {
		t.Fatalf("lookup calls = %d ids=%v", calls, callbackIDs)
	}
	if !reflect.DeepEqual(account.Summary, baseline.Summary) ||
		!reflect.DeepEqual(account.Records, baseline.Records) ||
		!reflect.DeepEqual(account.Pagination, baseline.Pagination) ||
		!reflect.DeepEqual(account.Series, baseline.Series) {
		t.Fatal("account labels changed ledger projection")
	}
	if len(account.Groups) != 2 {
		t.Fatalf("account groups = %+v", account.Groups)
	}
	wantLabels := map[string]string{"auth-a": "Alpha File", "auth-b": "Beta Label"}
	// Each read surface must project names once, without changing any other
	// response field or retaining host metadata in the accounting ledger.
	for _, route := range []string{"/requests", "/requests/view", "/requests/detail"} {
		query := url.Values{}
		switch route {
		case "/requests/view":
			query = url.Values{"from_date": {date}, "to_date": {date}, "view": {"records"}}
		case "/requests/detail":
			query.Set("id", "request-auth-a")
		}
		before := dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+route, query, "")
		calls = 0
		after := dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+route, query, "records-callback")
		if before.StatusCode != http.StatusOK || after.StatusCode != http.StatusOK || calls != 1 {
			t.Fatalf("%s projection status/calls: %d %d %d: %s", route, before.StatusCode, after.StatusCode, calls, after.Body)
		}
		var original, projected map[string]any
		if json.Unmarshal(before.Body, &original) != nil || json.Unmarshal(after.Body, &projected) != nil {
			t.Fatal("invalid response JSON")
		}
		var rows []any
		switch route {
		case "/requests":
			rows = projected["items"].([]any)
		case "/requests/view":
			rows = projected["records"].([]any)
		default:
			rows = []any{projected["record"]}
		}
		if len(rows) == 0 {
			t.Fatalf("%s returned no records", route)
		}
		for _, row := range rows {
			record := row.(map[string]any)
			if record["account_name"] != wantLabels[record["auth_index"].(string)] {
				t.Fatalf("%s name = %v", route, record)
			}
			delete(record, "account_name")
		}
		if !reflect.DeepEqual(original, projected) || strings.Contains(string(after.Body), "/not-stored") || strings.Contains(string(after.Body), "host-secret-not-returned") {
			t.Fatalf("%s changed non-name fields or leaked metadata", route)
		}
	}
	ledger, err := m.store.Requests(engine.Query{From: at.Add(-time.Second), To: now})
	if err != nil || len(ledger.Items) != 2 {
		t.Fatalf("ledger read: %+v %v", ledger, err)
	}
	for _, record := range ledger.Items {
		if record.AccountName != "" {
			t.Fatalf("ledger retained account name: %+v", record)
		}
	}
	// Unavailable metadata omits the optional name rather than returning an ID
	// or host error; empty pages must not invoke the host at all.
	lookup := m.accountMetadata
	m.SetAccountMetadataLookup(func(string) []byte { return []byte(`{"ok":false}`) })
	for _, route := range []string{"/requests", "/requests/view", "/requests/detail"} {
		query := url.Values{}
		if route == "/requests/detail" {
			query.Set("id", "request-auth-a")
		}
		before := dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+route, query, "")
		after := dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+route, query, "bad-metadata")
		if after.StatusCode != http.StatusOK || string(before.Body) != string(after.Body) || strings.Contains(string(after.Body), "account_name") {
			t.Fatalf("%s failed metadata fallback: %s", route, after.Body)
		}
	}
	m.SetAccountMetadataLookup(lookup)
	calls = 0
	empty := dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/view",
		url.Values{"view": {"records"}, "auth_index": {"absent"}}, "empty-page")
	if empty.StatusCode != http.StatusOK || calls != 0 {
		t.Fatalf("empty page lookup: %d calls=%d", empty.StatusCode, calls)
	}
	for _, group := range account.Groups {
		if group.Key != group.AuthIndex || wantLabels[group.AuthIndex] != group.Label {
			t.Fatalf("account group identity/label = %+v", group)
		}
	}

	// An empty callback id means the request did not originate from a host
	// management call; never invoke the bridge in that case.
	calls = 0
	response = dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", baseQuery, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("empty callback view: %d %s", response.StatusCode, response.Body)
	}
	if calls != 0 {
		t.Fatalf("lookup called without a host callback id: %d", calls)
	}

	// The same display name on two indexes must remain two distinct groups.
	m.SetAccountMetadataLookup(func(string) []byte {
		return accountMetadataPayload(t,
			map[string]any{"auth_index": "auth-a", "name": "Shared"},
			map[string]any{"auth_index": "auth-b", "name": "Shared"},
		)
	})
	var shared engine.RequestView
	response = dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", baseQuery, "callback-shared")
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &shared) != nil {
		t.Fatalf("shared-name view: %d %s", response.StatusCode, response.Body)
	}
	if len(shared.Groups) != 2 {
		t.Fatalf("duplicate names merged groups: %+v", shared.Groups)
	}
	seen := map[string]bool{}
	for _, group := range shared.Groups {
		seen[group.Key] = true
		if group.Label != "Shared" || group.AuthIndex != group.Key {
			t.Fatalf("shared-name group changed identity: %+v", group)
		}
	}
	if !seen["auth-a"] || !seen["auth-b"] {
		t.Fatalf("shared-name group keys = %v", seen)
	}

	// Restore the detailed payload for the remaining facet assertions.
	m.SetAccountMetadataLookup(func(callbackID string) []byte {
		calls++
		callbackIDs = append(callbackIDs, callbackID)
		return accountMetadataPayload(t,
			map[string]any{"auth_index": "auth-a", "name": "Alpha File", "label": "Alpha Label"},
			map[string]any{"auth_index": "auth-b", "name": "", "label": "Beta Label"},
			map[string]any{"auth_index": "auth-missing", "name": "Unused Account"},
		)
	})

	// Non-account groups and dimensions must not consult the metadata bridge.
	calls = 0
	for _, dimension := range []string{"provider", "model", "key_model"} {
		groupQuery := url.Values{"from_date": {date}, "to_date": {date}, "view": {dimension}}
		before := dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", groupQuery, "")
		got := dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", groupQuery, "callback-2")
		if got.StatusCode != http.StatusOK || string(got.Body) != string(before.Body) {
			t.Fatalf("%s view changed: %d %s", dimension, got.StatusCode, got.Body)
		}
	}
	if got := dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/facets", url.Values{"field": {"model"}}, "callback-3"); got.StatusCode != http.StatusOK {
		t.Fatalf("model facet: %d %s", got.StatusCode, got.Body)
	}
	if calls != 0 {
		t.Fatalf("lookup called outside account view/auth facet: %d", calls)
	}

	var first engine.FacetPage
	response = dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/facets", url.Values{"field": {"auth_index"}, "limit": {"1"}}, "callback-4")
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &first) != nil {
		t.Fatalf("auth facet: %d %s", response.StatusCode, response.Body)
	}
	if calls != 1 || len(first.Items) != 1 || first.Items[0].Value != "auth-a" || first.Items[0].Label != "Alpha File" {
		t.Fatalf("auth facet page = %+v calls=%d", first, calls)
	}
	cursor, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil || string(cursor) != "auth-a" {
		t.Fatalf("facet cursor = %q err=%v", cursor, err)
	}

	var search engine.FacetPage
	response = dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/facets",
		url.Values{"field": {"auth_index"}, "search": {"beta label"}}, "callback-5")
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &search) != nil {
		t.Fatalf("auth facet search: %d %s", response.StatusCode, response.Body)
	}
	if len(search.Items) != 1 || search.Items[0].Value != "auth-b" || search.Items[0].Label != "Beta Label" {
		t.Fatalf("label search = %+v", search)
	}

	var all engine.FacetPage
	response = dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/facets", url.Values{"field": {"auth_index"}}, "callback-6")
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &all) != nil {
		t.Fatalf("all auth facets: %d %s", response.StatusCode, response.Body)
	}
	if len(all.Items) != 2 {
		t.Fatalf("facets injected or dropped accounts: %+v", all)
	}

	// Missing or broken host metadata is silent and leaves index fallback.
	calls = 0
	m.SetAccountMetadataLookup(func(string) []byte {
		calls++
		return []byte(`{"ok":false,"error":{"code":"denied"}}`)
	})
	response = dispatchCallbackForTest(t, m, http.MethodGet, managementBasePath+"/requests/view", baseQuery, "callback-7")
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &account) != nil {
		t.Fatalf("bad metadata view: %d %s", response.StatusCode, response.Body)
	}
	for _, group := range account.Groups {
		if group.Label != group.AuthIndex || group.Key != group.AuthIndex {
			t.Fatalf("bad metadata did not fall back: %+v", group)
		}
	}
	if calls != 1 {
		t.Fatalf("bad metadata lookup calls = %d", calls)
	}
}

// Name -> Label -> index, control/oversize rejection and duplicate names on
// different indexes are pure metadata rules; they never merge identities.
func TestParseAccountLabelsNameRules(t *testing.T) {
	long := strings.Repeat("x", maxAccountNameBytes+1)
	atLimit := strings.Repeat("y", maxAccountNameBytes)
	labels := parseAccountLabels(accountMetadataPayload(t,
		map[string]any{"auth_index": "a", "name": " File A ", "label": "Label A"},
		map[string]any{"auth_index": "b", "name": "", "label": " Label B "},
		map[string]any{"auth_index": "c", "name": "bad\nname", "label": "Good C"},
		map[string]any{"auth_index": "d", "name": long, "label": ""},
		map[string]any{"auth_index": "e", "name": atLimit},
		map[string]any{"auth_index": "f", "name": "same"},
		map[string]any{"auth_index": "g", "name": "same"},
		map[string]any{"auth_index": "", "name": "missing index"},
	))
	if labels["a"] != "File A" || labels["b"] != "Label B" || labels["c"] != "Good C" || labels["e"] != atLimit {
		t.Fatalf("name/label priority = %+v", labels)
	}
	if _, found := labels["d"]; found {
		t.Fatal("oversized name accepted")
	}
	if labels["f"] != "same" || labels["g"] != "same" {
		t.Fatal("duplicate names on distinct indexes were not preserved")
	}
	if _, found := labels[""]; found {
		t.Fatal("missing index accepted")
	}

	for _, raw := range [][]byte{
		nil,
		[]byte(`not json`),
		[]byte(`{"ok":false,"error":{"code":"denied"}}`),
		[]byte(`{"ok":true,"result":null}`),
		make([]byte, maxAccountMetadataBytes+1),
	} {
		if got := parseAccountLabels(raw); got != nil {
			t.Fatalf("invalid metadata accepted: %s", got)
		}
	}
	if got := parseAccountLabels([]byte(`{"ok":true,"result":{"files":[{"auth_index":"a","name":"` + long + `"}]}}`)); got != nil {
		t.Fatalf("envelope with no usable name returned labels: %+v", got)
	}
}
