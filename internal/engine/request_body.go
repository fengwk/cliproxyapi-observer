package engine

import "time"

// admissionFact connects an admission to the pricing condition fingerprints
// observed in that request. It is deliberately in-memory only: restart or
// overflow removes the facts, making request-body conditions unknown rather
// than inventing a match. No body or raw field value is retained.
type admissionFact struct {
	RequestID string
	Started   time.Time
	At        time.Time
	// Conditions maps fingerprints to known results. At is retention metadata
	// and may be extended by later usage; Started is immutable and fences a
	// delayed usage record from a previous admission reusing the same trace.
	Conditions map[string]bool
	Ambiguous  bool
	Websocket  bool
	Rounds     []admissionRound // oldest first; WebSocket rounds are admitted sequentially
}

type admissionRound struct {
	RequestID  string
	Started    time.Time
	Conditions map[string]bool
}

const (
	maxAdmissionFacts  = maxCompleted
	maxWebsocketRounds = 64
)

func admissionFactID(key, trace string) string {
	return hash(key + "\x00" + trace)
}

func cloneConditions(in map[string]bool) map[string]bool {
	if in == nil {
		return nil
	}
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *Store) recordAdmissionFactsLocked(a Admission, conditions map[string]bool) {
	if a.TraceID == "" {
		return
	}
	s.pruneAdmissionFactsLocked()
	id := admissionFactID(a.KeyID, a.TraceID)
	now := s.now().UTC()
	started := a.At.UTC()
	if started.IsZero() {
		started = now
	}
	fact, exists := s.admissionFacts[id]
	switch {
	case a.Websocket && (!exists || fact.Websocket && !fact.Ambiguous):
		for _, round := range fact.Rounds {
			if round.RequestID == a.RequestID {
				return
			}
		}
		if len(fact.Rounds) == maxWebsocketRounds {
			copy(fact.Rounds, fact.Rounds[1:])
			fact.Rounds = fact.Rounds[:maxWebsocketRounds-1]
		}
		fact.Websocket = true
		fact.Rounds = append(fact.Rounds, admissionRound{a.RequestID, started, cloneConditions(conditions)})
	case !exists:
		fact = admissionFact{RequestID: a.RequestID, Started: started, Conditions: cloneConditions(conditions)}
	case fact.Websocket != a.Websocket || fact.RequestID != a.RequestID:
		// Distinct HTTP admissions or mixed transports sharing a trace
		// cannot be distinguished. Keep the WebSocket marker but never guess.
		fact = admissionFact{Ambiguous: true, Websocket: fact.Websocket || a.Websocket}
	default:
		return
	}
	fact.At = now
	s.admissionFacts[id] = fact
	s.pruneAdmissionFactsLocked()
}

func (s *Store) completeAdmissionFactsLocked(a Admission) {
	if a.TraceID == "" {
		return
	}
	id := admissionFactID(a.KeyID, a.TraceID)
	if fact, ok := s.admissionFacts[id]; ok && (fact.Websocket && a.Websocket || !fact.Ambiguous && fact.RequestID == a.RequestID) {
		fact.At = s.now().UTC()
		s.admissionFacts[id] = fact
	}
	s.pruneAdmissionFactsLocked()
}

func (s *Store) touchAdmissionFactsLocked(key, trace string) {
	if trace == "" {
		return
	}
	id := admissionFactID(key, trace)
	if fact, ok := s.admissionFacts[id]; ok && (fact.Websocket || !fact.Ambiguous) {
		fact.At = s.now().UTC()
		s.admissionFacts[id] = fact
	}
	s.pruneAdmissionFactsLocked()
}

func (fact admissionFact) conditionsAt(requestedAt time.Time) map[string]bool {
	if fact.Ambiguous {
		return nil
	}
	if fact.Websocket {
		// The host starts each usage reporter after admitting its round.
		// Use that start time, not the order in which usage arrives.
		for i := len(fact.Rounds) - 1; i >= 0; i-- {
			if !requestedAt.Before(fact.Rounds[i].Started) {
				return fact.Rounds[i].Conditions
			}
		}
		return nil
	}
	if fact.RequestID == "" {
		return nil
	}
	// An evicted slot can be reused by the same Key+Trace. A delayed usage
	// record from the earlier admission predates the new immutable start and
	// must stay unknown instead of borrowing the new request's facts.
	if !fact.Started.IsZero() && requestedAt.UTC().Before(fact.Started) {
		return nil
	}
	return fact.Conditions
}

func (s *Store) pruneAdmissionFactsLocked() {
	cutoff := s.now().UTC().Add(-completedTTL)
	total := 0
	for id, fact := range s.admissionFacts {
		if fact.At.Before(cutoff) {
			delete(s.admissionFacts, id)
			continue
		}
		total += max(1, len(fact.Rounds))
	}
	for total > maxAdmissionFacts {
		var oldest string
		var at time.Time
		for id, fact := range s.admissionFacts {
			if oldest == "" || fact.At.Before(at) {
				oldest = id
				at = fact.At
			}
		}
		total -= max(1, len(s.admissionFacts[oldest].Rounds))
		delete(s.admissionFacts, oldest)
	}
}
