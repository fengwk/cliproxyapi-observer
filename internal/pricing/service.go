package pricing

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const refreshInterval = 6 * time.Hour
const maxObserved = 4096

var errSourceChanged = errors.New("catalog source changed during refresh")

type sourceState struct {
	catalog   catalogIndex
	lastSync  time.Time
	lastError string
	loaded    bool
}

type flight struct {
	done chan struct{}
	err  error
}

type Service struct {
	mu                sync.Mutex
	config            Config
	requestConditions []RequestBodyCondition
	client            *http.Client
	cachePath         string
	sources           map[string]*sourceState
	generation        uint64
	observed          map[identity]struct{}
	flights           map[string]*flight
	queue             chan struct{}
	lastAttempt       time.Time
	running           bool
}

var _ Pricer = (*Service)(nil)

func NewService(c Config, cachePath string, client *http.Client) (*Service, error) {
	normalizeConfig(&c)
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
	s := &Service{
		config: cloneConfig(c), requestConditions: manualRequestBodyConditions(c),
		cachePath: cachePath, client: &h,
		sources: map[string]*sourceState{}, observed: map[identity]struct{}{},
		flights: map[string]*flight{}, queue: make(chan struct{}, 1),
	}
	s.loadSourceCacheLocked(s.config.Source)
	return s, nil
}

func normalizeConfig(c *Config) {
	if c.Source == "" {
		c.Source = SourceV2
	}
	if c.Prices == nil {
		c.Prices = []ManualPrice{}
	}
	if c.Mappings == nil {
		c.Mappings = []Mapping{}
	}
}

func (s *Service) Configure(c Config) error {
	normalizeConfig(&c)
	if err := Validate(c); err != nil {
		return err
	}
	c = cloneConfig(c)
	conditions := manualRequestBodyConditions(c)
	s.mu.Lock()
	wasAuto, wasSource := s.config.Auto, s.config.Source
	s.config = c
	s.requestConditions = conditions
	if wasSource != c.Source {
		s.generation++
		s.lastAttempt = time.Time{}
		s.loadSourceCacheLocked(c.Source)
	}
	s.mu.Unlock()
	if c.Auto && (!wasAuto || wasSource != c.Source) {
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
	k := identity{Model: model, Provider: provider}
	_, exists := s.observed[k]
	admitted := !exists && len(s.observed) < maxObserved
	if admitted {
		s.observed[k] = struct{}{}
	}
	miss := s.config.Auto && s.resolveLocked(model, provider).source == "zero"
	// One global cooldown bounds storms from many different missing IDs.
	queue := miss && admitted && time.Since(s.lastAttempt) >= time.Minute
	s.mu.Unlock()
	if queue {
		s.enqueue()
	}
}

func flightKey(source string, generation uint64) string {
	return source + "#" + strconv.FormatUint(generation, 10)
}

func (s *Service) Refresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	source, generation := s.config.Source, s.generation
	key := flightKey(source, generation)
	if f := s.flights[key]; f != nil {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.done:
			return f.err
		}
	}
	f := &flight{done: make(chan struct{})}
	s.flights[key] = f
	s.lastAttempt = time.Now()
	s.mu.Unlock()

	entries, err := s.fetch(ctx, source)
	var index catalogIndex
	if entries != nil {
		index = indexCatalog(entries)
	}
	now := time.Now().UTC()

	s.mu.Lock()
	if generation != s.generation || s.config.Source != source {
		if err == nil {
			err = errSourceChanged
		}
		f.err = err
		if s.flights[key] == f {
			delete(s.flights, key)
		}
		close(f.done)
		s.mu.Unlock()
		return err
	}
	state := s.sourceStateLocked(source)
	if entries != nil {
		state.catalog = index
		state.lastSync = now
		if saveErr := s.saveCacheLocked(source, entries, now); saveErr != nil {
			err = errors.New("catalog cache write failed")
		}
	}
	if err != nil {
		state.lastError = err.Error()
	} else {
		state.lastError = ""
	}
	f.err = err
	if s.flights[key] == f {
		delete(s.flights, key)
	}
	close(f.done)
	s.mu.Unlock()
	return err
}

func (s *Service) sourceStateLocked(source string) *sourceState {
	state := s.sources[source]
	if state == nil {
		state = &sourceState{}
		s.sources[source] = state
		s.loadSourceCacheLocked(source)
	}
	return state
}

func (s *Service) refreshingLocked() bool {
	key := flightKey(s.config.Source, s.generation)
	return s.flights[key] != nil
}

func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	state := s.sourceStateLocked(s.config.Source)
	delay := refreshInterval - time.Since(state.lastSync)
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
			auto := s.config.Auto && !s.refreshingLocked() && time.Since(s.lastAttempt) >= time.Minute
			s.mu.Unlock()
			if auto {
				_ = s.Refresh(ctx)
			}
		}
	}
}
