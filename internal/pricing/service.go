package pricing

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

const refreshInterval = 6 * time.Hour
const maxObserved = 4096

type flight struct {
	done chan struct{}
	err  error
}
type Service struct {
	mu          sync.Mutex
	config      Config
	client      *http.Client
	cachePath   string
	entries     []entry
	observed    map[identity]struct{}
	lastSync    time.Time
	lastError   string
	flight      *flight
	queue       chan struct{}
	lastAttempt time.Time
	running     bool
}

var _ Pricer = (*Service)(nil)

func NewService(c Config, cachePath string, client *http.Client) (*Service, error) {
	if err := Validate(c); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{}
	}
	// Copy rather than mutate the caller's client. Redirects may never leave
	// the trusted HTTPS host, even if the supplied client allows them.
	h := *client
	h.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || req.URL.Scheme != "https" || req.URL.Host != "models.dev" || req.URL.User != nil {
			return errors.New("catalog redirect rejected")
		}
		return nil
	}
	s := &Service{config: cloneConfig(c), cachePath: cachePath, client: &h, observed: map[identity]struct{}{}, queue: make(chan struct{}, 1)}
	s.loadCache()
	return s, nil
}
func (s *Service) Configure(c Config) error {
	if err := Validate(c); err != nil {
		return err
	}
	c = cloneConfig(c)
	s.mu.Lock()
	was := s.config.Auto
	s.config = c
	s.mu.Unlock()
	if c.Auto && !was {
		s.enqueue()
	}
	return nil
}
func (s *Service) enqueue() {
	select {
	case s.queue <- struct{}{}:
	default:
	}
}
func (s *Service) Observe(model, provider string) {
	if !validIdentity(model) || len(provider) > 1024 {
		return
	}
	s.mu.Lock()
	k := identity{model, provider}
	_, exists := s.observed[k]
	admitted := !exists && len(s.observed) < maxObserved
	if admitted {
		s.observed[k] = struct{}{}
	}
	miss := s.config.Auto && s.effective(model, provider).Source == "unpriced"
	// One global cooldown also bounds storms from many different missing IDs.
	queue := miss && admitted && time.Since(s.lastAttempt) >= time.Minute
	s.mu.Unlock()
	if queue {
		s.enqueue()
	}
}
func (s *Service) Refresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if f := s.flight; f != nil {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.done:
			return f.err
		}
	}
	f := &flight{done: make(chan struct{})}
	s.flight = f
	s.lastAttempt = time.Now()
	s.mu.Unlock()
	entries, err := s.fetch(ctx)
	now := time.Now().UTC()
	if err == nil {
		if e := s.saveCache(diskCache{LastSync: now, Entries: entries}); e != nil {
			err = errors.New("catalog cache write failed")
		}
	}
	s.mu.Lock()
	if entries != nil {
		s.entries = entries
		s.lastSync = now
	}
	s.lastError = ""
	if err != nil {
		s.lastError = err.Error()
	}
	f.err = err
	s.flight = nil
	close(f.done)
	s.mu.Unlock()
	return err
}
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	delay := refreshInterval - time.Since(s.lastSync)
	if delay < 0 {
		delay = 0
	}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.enqueue()
			timer.Reset(refreshInterval)
		case <-s.queue:
			s.mu.Lock()
			// Discard redundant queued misses after a refresh, including failures.
			auto := s.config.Auto && s.flight == nil && time.Since(s.lastAttempt) >= time.Minute
			s.mu.Unlock()
			if auto {
				_ = s.Refresh(ctx)
			}
		}
	}
}
