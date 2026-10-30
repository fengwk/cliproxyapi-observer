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
}

const maxAdmissionFacts = maxCompleted

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
	if old, ok := s.admissionFacts[id]; ok {
		if old.RequestID != a.RequestID {
			// Two distinct admissions sharing trace correlation cannot be
			// distinguished at usage time. Never guess between them.
			old.RequestID = ""
			old.Conditions = nil
			old.Ambiguous = true
			old.At = s.now().UTC()
			s.admissionFacts[id] = old
		}
		return
	}
	started := a.At.UTC()
	if started.IsZero() {
		started = s.now().UTC()
	}
	s.admissionFacts[id] = admissionFact{
		RequestID:  a.RequestID,
		Started:    started,
		At:         s.now().UTC(),
		Conditions: cloneConditions(conditions),
	}
	s.pruneAdmissionFactsLocked()
}

func (s *Store) completeAdmissionFactsLocked(a Admission) {
	if a.TraceID == "" {
		return
	}
	id := admissionFactID(a.KeyID, a.TraceID)
	if fact, ok := s.admissionFacts[id]; ok && fact.RequestID == a.RequestID && !fact.Ambiguous {
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
	if fact, ok := s.admissionFacts[id]; ok && !fact.Ambiguous {
		fact.At = s.now().UTC()
		s.admissionFacts[id] = fact
	}
	s.pruneAdmissionFactsLocked()
}

func (s *Store) admissionConditionsLocked(key, trace string, requestedAt time.Time) map[string]bool {
	if trace == "" {
		return nil
	}
	s.pruneAdmissionFactsLocked()
	fact, ok := s.admissionFacts[admissionFactID(key, trace)]
	if !ok || fact.Ambiguous || fact.RequestID == "" {
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
	for id, fact := range s.admissionFacts {
		if fact.At.Before(cutoff) {
			delete(s.admissionFacts, id)
		}
	}
	for len(s.admissionFacts) > maxAdmissionFacts {
		var oldest string
		var at time.Time
		for id, fact := range s.admissionFacts {
			if oldest == "" || fact.At.Before(at) {
				oldest = id
				at = fact.At
			}
		}
		delete(s.admissionFacts, oldest)
	}
}
