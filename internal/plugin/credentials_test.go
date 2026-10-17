package plugin

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"net/http"
	"net/url"
	"testing"
)

// The host sends PascalCase SDK fields, not a custom lower_snake_case schema.
// Verify all credential sources and ambiguity rejection without echoing secrets.
func TestCredentials(t *testing.T) {
	for _, req := range []pluginapi.FrontendAuthRequest{
		{Headers: http.Header{"Authorization": {"Bearer fake"}}},
		{Headers: http.Header{"x-api-key": {"fake"}}},
		{Headers: http.Header{"X-Goog-Api-Key": {"fake"}}, Query: url.Values{"key": {"fake"}}},
		{Query: url.Values{"auth_token": {"fake"}}},
	} {
		if got, err := credential(req); err != nil || got != "fake" {
			t.Fatal("credential extraction failed")
		}
	}
	for _, req := range []pluginapi.FrontendAuthRequest{
		{}, {Headers: http.Header{"Authorization": {"Basic fake"}}},
		{Headers: http.Header{"Authorization": {"Bearer fake"}, "X-Api-Key": {"different"}}},
		{Query: url.Values{"key": {"fake", "different"}}},
	} {
		if _, err := credential(req); err == nil {
			t.Fatal("ambiguous/missing credentials accepted")
		}
	}
}
