package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxCatalogBytes = 32 << 20
const catalogURL = "https://models.dev/api.json"

type identity struct {
	Model    string
	Provider string
}
type entry struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Rates    Rates  `json:"rates"`
}
type diskCache struct {
	LastSync time.Time `json:"last_sync"`
	Entries  []entry   `json:"entries"`
}

// Catalog prices are decimal USD/million. Convert exactly, rounding fractional
// micro-USD upward rather than involving binary floating point in billing.
func catalogRate(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	// Bound decimal exponents before big.Rat allocates their expanded value.
	if len(n) > 64 {
		return 0, errInvalid
	}
	if i := strings.IndexAny(string(n), "eE"); i >= 0 {
		e, err := strconv.Atoi(string(n)[i+1:])
		if err != nil || e < -30 || e > 18 {
			return 0, errInvalid
		}
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || r.Sign() < 0 {
		return 0, errInvalid
	}
	r.Mul(r, big.NewRat(1000000, 1))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > maxSafe {
		return 0, errInvalid
	}
	return q.Int64(), nil
}
func parseCatalog(body []byte) ([]entry, error) {
	var root map[string]struct {
		Models map[string]struct {
			Cost *struct {
				Input      json.Number `json:"input"`
				Output     json.Number `json:"output"`
				CacheRead  json.Number `json:"cache_read"`
				CacheWrite json.Number `json:"cache_write"`
				Reasoning  json.Number `json:"reasoning"`
			} `json:"cost"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &root) != nil || len(root) == 0 {
		return nil, errInvalid
	}
	out := []entry{}
	for provider, p := range root {
		if !validIdentity(provider) {
			return nil, errInvalid
		}
		for model, m := range p.Models {
			if !validIdentity(model) {
				return nil, errInvalid
			}
			if m.Cost == nil {
				continue
			}
			if m.Cost.Input == "" && m.Cost.Output == "" && m.Cost.CacheRead == "" && m.Cost.CacheWrite == "" && m.Cost.Reasoning == "" {
				continue
			}
			values := []json.Number{m.Cost.Input, m.Cost.Output, m.Cost.CacheRead, m.Cost.CacheWrite}
			r := Rates{}
			dst := []*int64{&r.Input, &r.Output, &r.CacheRead, &r.CacheWrite}
			for i, n := range values {
				v, err := catalogRate(n)
				if err != nil {
					return nil, err
				}
				*dst[i] = v
			}
			if m.Cost.Reasoning != "" {
				v, err := catalogRate(m.Cost.Reasoning)
				if err != nil {
					return nil, err
				}
				r.Reasoning = &v
			}
			out = append(out, entry{Model: model, Provider: provider, Rates: r})
		}
	}
	if len(out) == 0 || len(out) > 100000 {
		return nil, errInvalid
	}
	return out, nil
}
func ratesEqual(a, b Rates) bool {
	return a.Input == b.Input && a.Output == b.Output && a.CacheRead == b.CacheRead && a.CacheWrite == b.CacheWrite &&
		((a.Reasoning == nil && b.Reasoning == nil) || (a.Reasoning != nil && b.Reasoning != nil && *a.Reasoning == *b.Reasoning))
}

type catalogMatch struct {
	rates     Rates
	ambiguous bool
}
type catalogIndex map[identity]catalogMatch

// Build once per catalog replacement. The empty-provider key summarizes all
// providers, retaining ambiguity instead of picking a map iteration winner.
func indexCatalog(entries []entry) catalogIndex {
	index := make(catalogIndex, len(entries)*2)
	for _, e := range entries {
		for _, provider := range []string{e.Provider, ""} {
			k := identity{Model: e.Model, Provider: provider}
			old, found := index[k]
			if !found {
				index[k] = catalogMatch{rates: cloneRates(e.Rates)}
			} else if !ratesEqual(old.rates, e.Rates) {
				old.ambiguous = true
				index[k] = old
			}
		}
	}
	return index
}
func normalize(model string) string {
	// Only explicit transport decoration is removed; dates/version suffixes
	// are model identities, not aliases.
	for _, prefix := range []string{"openai/", "anthropic/", "google/", "models/"} {
		if strings.HasPrefix(model, prefix) {
			model = strings.TrimPrefix(model, prefix)
			break
		}
	}
	for _, suffix := range []string{"(thinking)", "(high)", "(medium)", "(low)"} {
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix)
		}
	}
	return model
}
func lookup(index catalogIndex, model, provider string) (Rates, bool) {
	// A provider-specific exact identity is authoritative even when normalized
	// identities exist elsewhere. Ambiguous exact matches must not fall back.
	for _, candidate := range []string{model, normalize(model)} {
		for _, p := range []string{provider, ""} {
			if match, found := index[identity{Model: candidate, Provider: p}]; found {
				if match.ambiguous {
					return Rates{}, false
				}
				return match.rates, true
			}
		}
	}
	return Rates{}, false
}
func (s *Service) fetch(ctx context.Context) ([]entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return nil, errors.New("catalog request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("catalog HTTP status rejected")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxCatalogBytes+1))
	if err != nil || len(body) > maxCatalogBytes {
		return nil, errors.New("catalog read failed or exceeded size limit")
	}
	entries, err := parseCatalog(body)
	if err != nil {
		return nil, errors.New("catalog format invalid")
	}
	return entries, nil
}
func (s *Service) loadCache() {
	if s.cachePath == "" {
		return
	}
	f, err := os.Open(s.cachePath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		s.lastError = "catalog cache read failed"
		return
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	var d diskCache
	if err != nil || len(body) > maxCatalogBytes || json.Unmarshal(body, &d) != nil || len(d.Entries) == 0 || len(d.Entries) > 100000 || d.LastSync.IsZero() {
		s.lastError = "catalog cache invalid"
		return
	}
	seen := map[identity]bool{}
	for _, e := range d.Entries {
		k := identity{e.Model, e.Provider}
		if !validIdentity(e.Model) || !validIdentity(e.Provider) || !validRates(e.Rates) || seen[k] {
			s.lastError = "catalog cache invalid"
			return
		}
		seen[k] = true
	}
	s.catalog = indexCatalog(d.Entries)
	s.lastSync = d.LastSync
}
func (s *Service) saveCache(d diskCache) error {
	if s.cachePath == "" {
		return nil
	}
	body, err := json.Marshal(d)
	if err != nil || len(body) > maxCatalogBytes {
		return errors.New("catalog cache too large")
	}
	dir := filepath.Dir(s.cachePath)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pricing-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.cachePath)
}
