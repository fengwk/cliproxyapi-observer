package engine

import (
	"fmt"
	"testing"
	"time"
)

// Admission and Usage carry different request IDs. The Key+Trace association
// must survive completion so asynchronous usage still sees the snapshotted
// condition result, while another Key with the same trace does not.
func TestAdmissionConditionAssociationAndKeyIsolation(t *testing.T) {
	s, now, p := fixture(t)
	p.snapshot = map[string]bool{"fingerprint": true}
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.AdmitWithBody(Admission{RequestID: "admission", TraceID: "trace", KeyID: k.ID, Model: "gpt-exact", At: *now}, []byte(`{"speed":"fast"}`)))
	must(t, s.Complete("admission", *now))

	u := event(k, "usage", *now)
	u.TraceID = "trace"
	must(t, s.RecordUsage(u))
	if p.facts["fingerprint"] != true {
		t.Fatalf("quote did not receive admission condition: %v", p.facts)
	}
	d, e := s.Detail("usage")
	must(t, e)
	if d.Record.CostMicroUSD != 7 || d.CaptureStatus != captureStatusDisabled {
		t.Fatalf("usage/detail = %+v", d)
	}

	other, _ := key(t, s, KeySpec{Enabled: true})
	u.RequestID, u.APIKey, u.TraceID = "cross-key", "observer:"+other.ID, "trace"
	must(t, s.RecordUsage(u))
	if p.facts != nil {
		t.Fatalf("cross-key admission leaked conditions: %v", p.facts)
	}
}

func TestAdmissionConditionCollisionIsUnknown(t *testing.T) {
	s, now, p := fixture(t)
	p.snapshot = map[string]bool{"fingerprint": true}
	k, _ := key(t, s, KeySpec{Enabled: true})
	base := Admission{TraceID: "collision", KeyID: k.ID, Model: "gpt-exact", At: *now}
	base.RequestID = "admission-a"
	must(t, s.AdmitWithBody(base, []byte(`{"speed":"fast"}`)))
	base.RequestID = "admission-b"
	must(t, s.AdmitWithBody(base, []byte(`{"speed":"slow"}`)))
	must(t, s.Complete("admission-a", *now))
	must(t, s.Complete("admission-b", *now))

	u := event(k, "usage-after-collision", *now)
	u.TraceID = "collision"
	must(t, s.RecordUsage(u))
	if p.facts != nil {
		t.Fatalf("ambiguous correlation guessed facts: %v", p.facts)
	}
}

func TestAdmissionConditionRetentionAndMissingNeverGates(t *testing.T) {
	s, now, p := fixture(t)
	p.snapshot = map[string]bool{"fingerprint": true}
	k, _ := key(t, s, KeySpec{Enabled: true})
	must(t, s.AdmitWithBody(Admission{RequestID: "old", TraceID: "expired", KeyID: k.ID, Model: "gpt-exact", At: *now}, []byte(`{}`)))
	must(t, s.Complete("old", *now))
	*now = now.Add(completedTTL + time.Hour)

	u := event(k, "expired-usage", now.Add(-time.Minute))
	u.TraceID = "expired"
	must(t, s.RecordUsage(u))
	if p.facts != nil {
		t.Fatalf("expired correlation reused facts: %v", p.facts)
	}
	if got := total(t, s); got.Used.Total != 7 || got.Active != 0 {
		t.Fatalf("missing facts gated accounting/admission: %+v", got)
	}
}

func TestAdmissionFactCapacityIsBounded(t *testing.T) {
	s, now, _ := fixture(t)
	s.mu.Lock()
	for i := 0; i < maxAdmissionFacts+1; i++ {
		s.admissionFacts[fmt.Sprintf("fact-%d", i)] = admissionFact{RequestID: "a", At: *now}
	}
	s.pruneAdmissionFactsLocked()
	got := len(s.admissionFacts)
	s.mu.Unlock()
	if got != maxAdmissionFacts {
		t.Fatalf("admission fact capacity = %d, want %d", got, maxAdmissionFacts)
	}
}

func TestAdmissionFactReuseRejectsDelayedEarlierUsage(t *testing.T) {
	s, now, p := fixture(t)
	p.snapshot = map[string]bool{"fingerprint": true}
	k, _ := key(t, s, KeySpec{Enabled: true})
	start := *now

	must(t, s.AdmitWithBody(Admission{RequestID: "old", TraceID: "reused-trace", KeyID: k.ID, Model: "gpt-exact", At: start}, []byte(`{}`)))
	must(t, s.Complete("old", start))
	*now = start.Add(completedTTL + time.Hour)
	must(t, s.AdmitWithBody(Admission{RequestID: "new", TraceID: "reused-trace", KeyID: k.ID, Model: "gpt-exact", At: *now}, []byte(`{}`)))
	must(t, s.Complete("new", *now))

	oldUsage := event(k, "delayed-old-usage", start)
	oldUsage.TraceID = "reused-trace"
	must(t, s.RecordUsage(oldUsage))
	if p.facts != nil {
		t.Fatalf("delayed earlier usage borrowed reused admission facts: %v", p.facts)
	}

	newUsage := event(k, "new-usage", *now)
	newUsage.TraceID = "reused-trace"
	must(t, s.RecordUsage(newUsage))
	if p.facts["fingerprint"] != true {
		t.Fatalf("current admission lost facts after reuse: %v", p.facts)
	}
}

func TestAdmissionFactEvictionReuseRejectsEarlierUsage(t *testing.T) {
	s, now, p := fixture(t)
	p.snapshot = map[string]bool{"fingerprint": true}
	k, _ := key(t, s, KeySpec{Enabled: true})
	start := *now

	must(t, s.AdmitWithBody(Admission{RequestID: "evicted", TraceID: "evicted-trace", KeyID: k.ID, Model: "gpt-exact", At: start}, []byte(`{}`)))
	must(t, s.Complete("evicted", start))
	// Fill the bounded cache so the old entry is the eviction victim before the
	// same Key+Trace is admitted again.
	s.mu.Lock()
	for i := 0; i < maxAdmissionFacts; i++ {
		s.admissionFacts[fmt.Sprintf("filler-%d", i)] = admissionFact{
			RequestID: "filler", Started: start.Add(time.Hour), At: start.Add(time.Hour),
		}
	}
	s.mu.Unlock()
	reusedAt := start.Add(2 * time.Hour)
	*now = reusedAt
	must(t, s.AdmitWithBody(Admission{RequestID: "reused", TraceID: "evicted-trace", KeyID: k.ID, Model: "gpt-exact", At: reusedAt}, []byte(`{}`)))
	must(t, s.Complete("reused", reusedAt))

	oldUsage := event(k, "evicted-old-usage", start)
	oldUsage.TraceID = "evicted-trace"
	must(t, s.RecordUsage(oldUsage))
	if p.facts != nil {
		t.Fatalf("evicted admission facts reused by earlier usage: %v", p.facts)
	}
}
