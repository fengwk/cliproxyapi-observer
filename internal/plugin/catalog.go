package plugin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const catalogTokenTTL = 30 * time.Second
const maxCatalogTokens = 32

type catalogToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// The host management middleware protects issuance. These are short-lived
// capabilities, never engine Keys and never usable by admission or usage.
func (m *Manager) serveCatalogToken() pluginapi.ManagementResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.store == nil || m.closed {
		return unavailableResponse()
	}
	m.capsMu.Lock()
	defer m.capsMu.Unlock()
	now := m.currentTime()
	m.pruneCatalog(now)
	if len(m.catalogCaps) >= maxCatalogTokens {
		return errorResponse(429, "capacity", "catalog token capacity reached")
	}
	var material [32]byte
	if _, err := rand.Read(material[:]); err != nil {
		return errorResponse(503, "unavailable", "catalog token unavailable")
	}
	token := base64.RawURLEncoding.EncodeToString(material[:])
	expires := now.Add(catalogTokenTTL)
	if m.catalogCaps == nil {
		m.catalogCaps = make(map[[32]byte]time.Time)
	}
	m.catalogCaps[sha256.Sum256([]byte(token))] = expires
	return jsonResponse(200, catalogToken{token, expires})
}

// Caller holds m.mu.RLock, so close cannot clear/release concurrently.
func (m *Manager) consumeCatalog(r pluginapi.FrontendAuthRequest, secret string) bool {
	m.capsMu.Lock()
	defer m.capsMu.Unlock()
	m.pruneCatalog(m.currentTime())
	hash := sha256.Sum256([]byte(secret))
	if _, ok := m.catalogCaps[hash]; !ok {
		return false
	}
	if r.Method != http.MethodGet || r.Path != "/v1/models" || len(r.Query["key"]) != 0 || len(r.Query["auth_token"]) != 0 {
		return false
	}
	bearer := false
	for name, values := range r.Headers {
		if strings.EqualFold(name, "x-api-key") || strings.EqualFold(name, "x-goog-api-key") {
			return false
		}
		if strings.EqualFold(name, "Authorization") {
			if len(values) != 1 {
				return false
			}
			parts := strings.Fields(values[0])
			bearer = len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && parts[1] == secret
		}
	}
	if !bearer {
		return false
	}
	delete(m.catalogCaps, hash)
	return true
}

func (m *Manager) pruneCatalog(now time.Time) {
	for hash, expiry := range m.catalogCaps {
		if !now.Before(expiry) {
			delete(m.catalogCaps, hash)
		}
	}
}
