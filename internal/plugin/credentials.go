package plugin

import (
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// credential accepts identical duplicates, but never lets one credential source
// silently override another. Header names are compared case-insensitively even
// for noncanonical JSON RPC input.
func credential(r pluginapi.FrontendAuthRequest) (string, error) {
	var secret string
	add := func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" || (secret != "" && value != secret) {
			return errors.New("invalid credentials")
		}
		secret = value
		return nil
	}
	for name, values := range r.Headers {
		switch {
		case strings.EqualFold(name, "Authorization"):
			for _, value := range values {
				parts := strings.Fields(value)
				if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
					return "", errors.New("invalid credentials")
				}
				if err := add(parts[1]); err != nil {
					return "", err
				}
			}
		case strings.EqualFold(name, "x-api-key"), strings.EqualFold(name, "x-goog-api-key"):
			for _, value := range values {
				if err := add(value); err != nil {
					return "", err
				}
			}
		}
	}
	for _, name := range []string{"key", "auth_token"} {
		for _, value := range r.Query[name] {
			if err := add(value); err != nil {
				return "", err
			}
		}
	}
	if secret == "" {
		return "", errors.New("missing credentials")
	}
	return secret, nil
}
