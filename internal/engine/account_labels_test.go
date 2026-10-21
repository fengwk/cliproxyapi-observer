package engine

import (
	"encoding/base64"
	"testing"
)

// Account display names are injected only into auth_index facets that already
// exist in retained records. Search sees the label, while value, ordering and
// cursor remain the stable AuthIndex.
func TestAuthIndexFacetLabelsSearchAndCursor(t *testing.T) {
	s, now, _ := fixture(t)
	k, _ := key(t, s, KeySpec{Enabled: true, Label: "Client"})
	for _, index := range []string{"auth-a", "auth-b"} {
		u := event(k, "request-"+index, *now)
		u.AuthIndex = index
		must(t, s.RecordUsage(u))
	}
	labels := map[string]string{
		"auth-a":       "Primary Account",
		"auth-b":       "Shared Account",
		"auth-missing": "Ghost Account",
	}

	first, e := s.Facets(FacetQuery{Field: facetAuthIndex, Limit: 1, Labels: labels})
	must(t, e)
	if len(first.Items) != 1 || first.Items[0].Value != "auth-a" || first.Items[0].Label != "Primary Account" {
		t.Fatalf("first labeled facet page = %+v", first)
	}
	cursor, e := base64.RawURLEncoding.DecodeString(first.NextCursor)
	must(t, e)
	if string(cursor) != "auth-a" {
		t.Fatalf("cursor identity = %q, want auth-a", cursor)
	}

	second, e := s.Facets(FacetQuery{Field: facetAuthIndex, Limit: 1, Cursor: first.NextCursor, Labels: labels})
	must(t, e)
	if len(second.Items) != 1 || second.Items[0].Value != "auth-b" || second.Items[0].Label != "Shared Account" || second.NextCursor != "" {
		t.Fatalf("second labeled facet page = %+v", second)
	}

	search, e := s.Facets(FacetQuery{Field: facetAuthIndex, Search: "shared", Labels: labels})
	must(t, e)
	if len(search.Items) != 1 || search.Items[0].Value != "auth-b" || search.Items[0].Label != "Shared Account" {
		t.Fatalf("label search = %+v", search)
	}
	byIndex, e := s.Facets(FacetQuery{Field: facetAuthIndex, Search: "auth-a", Labels: labels})
	must(t, e)
	if len(byIndex.Items) != 1 || byIndex.Items[0].Value != "auth-a" {
		t.Fatalf("index search = %+v", byIndex)
	}

	plain, e := s.Facets(FacetQuery{Field: facetAuthIndex})
	must(t, e)
	if len(plain.Items) != 2 {
		t.Fatalf("unlabeled facet page = %+v", plain)
	}
	for _, item := range plain.Items {
		if item.Value != item.Label {
			t.Fatalf("unlabeled facet value/label diverged: %+v", item)
		}
		if item.Value == "auth-missing" {
			t.Fatal("facet injected an account with no retained records")
		}
	}

	// Labels are scoped to auth_index; other dimensions must not change.
	model, e := s.Facets(FacetQuery{Field: facetModel, Labels: map[string]string{"gpt-exact": "Renamed"}})
	must(t, e)
	if len(model.Items) != 1 || model.Items[0].Label != "gpt-exact" {
		t.Fatalf("model facet unexpectedly relabeled: %+v", model)
	}
}
