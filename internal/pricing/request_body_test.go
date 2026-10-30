package pricing

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func bodyCondition(t *testing.T, path string, value JSONScalar) RequestBodyCondition {
	t.Helper()
	c := RequestBodyCondition{Path: path, Equals: value}
	if !c.valid() {
		t.Fatalf("condition rejected: %q %q", path, value.Kind())
	}
	return c
}

func numberScalar(t *testing.T, text string) JSONScalar {
	t.Helper()
	v, err := JSONNumber(text)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The reviewed vectors cover exact scalar typing, null presence, RFC 6901
// escapes, array indices, numeric equality and non-scalar values.
func TestRequestBodyVectors(t *testing.T) {
	cases := []struct {
		path   string
		equals JSONScalar
		body   string
		match  bool
	}{
		{"/speed", JSONString("fast"), `{"speed":"fast"}`, true},
		{"/speed", JSONString("fast"), `{"speed":"FAST"}`, false},
		{"/speed", JSONNull(), `{}`, false},
		{"/speed", JSONNull(), `{"speed":null}`, true},
		{"/a~1b/~0mode", JSONBool(true), `{"a/b":{"~mode":true}}`, true},
		{"/options/0/mode", JSONString("x"), `{"options":[{"mode":"x"}]}`, true},
		{"/n", numberScalar(t, "2.0"), `{"n":2}`, true},
		{"/n", numberScalar(t, "2"), `{"n":"2"}`, false},
		{"/", JSONBool(false), `{"":false}`, true},
		{"/n", JSONString("x"), `{"n":{}}`, false},
	}
	for _, tc := range cases {
		condition := bodyCondition(t, tc.path, tc.equals)
		got := snapshotRequestBody([]byte(tc.body), []RequestBodyCondition{condition})
		if got == nil {
			t.Fatalf("valid body rejected: %s", tc.body)
		}
		if got[condition.Fingerprint()] != tc.match {
			t.Fatalf("%s %s = %v, want %v", tc.path, tc.body, got[condition.Fingerprint()], tc.match)
		}
	}
}

func TestRequestBodyMissingAndScalarTraversalAreFalse(t *testing.T) {
	conditions := []RequestBodyCondition{
		bodyCondition(t, "/missing", JSONString("x")),
		bodyCondition(t, "/missing/deep", JSONString("x")),
		bodyCondition(t, "/scalar/child", JSONString("x")),
		bodyCondition(t, "/object", JSONString("x")),
	}
	got := snapshotRequestBody([]byte(`{"scalar":1,"object":{"a":1}}`), conditions)
	if got == nil {
		t.Fatal("valid body treated as unknown")
	}
	for _, condition := range conditions {
		if got[condition.Fingerprint()] {
			t.Fatalf("unexpected match: %s", condition.Path)
		}
	}
	// A scalar at the leaf path is also a known false for a non-scalar actual
	// value, never an unknown fact.
	scalarLeaf := bodyCondition(t, "/value", JSONString("x"))
	got = snapshotRequestBody([]byte(`{"value":[1,2]}`), []RequestBodyCondition{scalarLeaf})
	if got == nil || got[scalarLeaf.Fingerprint()] {
		t.Fatalf("non-scalar leaf = %v", got)
	}
}

func TestRequestBodyInvalidBodiesAreUnknown(t *testing.T) {
	condition := bodyCondition(t, "/x", JSONString("y"))
	invalid := []string{
		`{"x":"y"`,
		`{"x":"y"} trailing`,
		`{"x":"y","x":"y"}`,
		`{"outer":{"x":"y","x":"y"}}`,
		`{"x":"y"} {}`,
	}
	for _, body := range invalid {
		if got := snapshotRequestBody([]byte(body), []RequestBodyCondition{condition}); got != nil {
			t.Fatalf("invalid body accepted: %s", body)
		}
	}
	oversized := make([]byte, maxRequestJSONBytes+1)
	if got := snapshotRequestBody(oversized, []RequestBodyCondition{condition}); got != nil {
		t.Fatal("oversized body accepted")
	}

	accepted := strings.Repeat("[", maxRequestJSONDepth) + "0" + strings.Repeat("]", maxRequestJSONDepth)
	deep := bodyCondition(t, "/x", JSONString("y"))
	if got := snapshotRequestBody([]byte(accepted), []RequestBodyCondition{deep}); got == nil {
		t.Fatal("depth 64 rejected")
	}
	tooDeep := "[" + accepted + "]"
	if got := snapshotRequestBody([]byte(tooDeep), []RequestBodyCondition{deep}); got != nil {
		t.Fatal("depth 65 accepted")
	}
}

func TestRequestBodyPathAndNumberBounds(t *testing.T) {
	validPaths := []string{"/", "/a", "/a/", "/a//b", "/a~1b/~0mode"}
	for _, path := range validPaths {
		if _, ok := decodePointer(path); !ok {
			t.Fatalf("valid pointer rejected: %q", path)
		}
	}
	tooMany := "/" + strings.Repeat("a/", maxRequestBodySegments) + "a"
	invalidPaths := []string{"", "a", "/a~2b", "/a~", tooMany, "/" + strings.Repeat("a", maxRequestBodyPathBytes)}
	for _, path := range invalidPaths {
		if _, ok := decodePointer(path); ok {
			t.Fatalf("invalid pointer accepted: %q", path)
		}
	}

	validNumbers := map[string]string{
		"0": "0", "-0.0": "0", "2.0": "2", "1e2": "100",
		"1e-18": "0.000000000000000001",
		"12300": "12300", strings.Repeat("9", 64): strings.Repeat("9", 64),
	}
	for in, want := range validNumbers {
		got, err := JSONNumber(in)
		if err != nil || got.Text() != want {
			t.Fatalf("number %q => %q, %v; want %q", in, got.Text(), err, want)
		}
	}
	invalidNumbers := []string{"01", "1.", ".1", "1e-19", "1e19", strings.Repeat("9", 65), "1e999999999999999999"}
	for _, in := range invalidNumbers {
		if _, err := JSONNumber(in); err == nil {
			t.Fatalf("invalid number accepted: %q", in)
		}
	}
}

func TestRequestBodyFingerprintIncludesTypeAndValue(t *testing.T) {
	a := bodyCondition(t, "/n", numberScalar(t, "2"))
	b := bodyCondition(t, "/n", numberScalar(t, "2.0"))
	c := bodyCondition(t, "/n", JSONString("2"))
	d := bodyCondition(t, "/n", JSONBool(true))
	e := bodyCondition(t, "/m", numberScalar(t, "2"))
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("equivalent numbers have different fingerprints")
	}
	seen := map[string]bool{}
	for _, condition := range []RequestBodyCondition{a, c, d, e} {
		if seen[condition.Fingerprint()] {
			t.Fatal("distinct condition fingerprints collided")
		}
		seen[condition.Fingerprint()] = true
	}
	if len(a.Fingerprint()) != 64 || strings.ToLower(a.Fingerprint()) != a.Fingerprint() {
		t.Fatal("fingerprint is not lowercase sha256")
	}
}

func TestRequestBodySnapshotCapAndManualOnly(t *testing.T) {
	base := int64(1_000_000)
	over := int64(5_000_000)
	prices := make([]ManualPrice, 0, 3)
	for group := 0; group < 3; group++ {
		overrides := make([]Override, 0, 43)
		for i := group * 43; i < (group+1)*43 && i < maxRequestConditions+1; i++ {
			overrides = append(overrides, Override{
				ID: "r" + strconv.Itoa(i),
				When: Condition{RequestBody: &RequestBodyCondition{
					Path: "/p" + strconv.Itoa(i), Equals: JSONString("v"),
				}},
				Rates: RatePatch{Input: &over},
			})
		}
		prices = append(prices, ManualPrice{
			Model: "m" + strconv.Itoa(group), Definition: PricingDefinition{Base: RateSet{Input: base}, Overrides: overrides},
		})
	}
	config := Config{Prices: prices}
	if err := Validate(config); err != nil {
		t.Fatal(err)
	}
	conditions := manualRequestBodyConditions(config)
	if len(conditions) != maxRequestConditions {
		t.Fatalf("conditions = %d, want %d", len(conditions), maxRequestConditions)
	}

	s, err := NewService(config, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.SnapshotConditions([]byte("{invalid")); got != nil {
		t.Fatal("partial/invalid body produced snapshots")
	}
	body := []byte(`{"p0":"v","p128":"v"}`)
	snapshot := s.SnapshotConditions(body)
	if len(snapshot) != maxRequestConditions {
		t.Fatalf("snapshot = %d entries", len(snapshot))
	}
	first := conditions[0].Fingerprint()
	if !snapshot[first] {
		t.Fatal("first bounded condition not evaluated")
	}
}

func TestQuoteRequestConditionTrueFalseUnknown(t *testing.T) {
	base := int64(1_000_000)
	fast := int64(5_000_000)
	condition := bodyCondition(t, "/speed", JSONString("fast"))
	config := Config{Prices: []ManualPrice{{
		Model: "m",
		Definition: PricingDefinition{
			Base: RateSet{Input: base},
			Overrides: []Override{{
				ID: "fast", When: Condition{RequestBody: &condition}, Rates: RatePatch{Input: &fast},
			}},
		},
	}}}
	s, err := NewService(config, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	tokens := Tokens{Input: 1, Total: 1}
	snapshot := s.SnapshotConditions([]byte(`{"speed":"fast"}`))
	q, err := s.QuoteWithFacts("m", "", UsageFacts{Tokens: tokens, RequestedAt: at, Conditions: snapshot})
	if err != nil || q.CostMicroUSD != 5 || q.Reason == reasonMissingFacts {
		t.Fatalf("true condition quote = %+v, %v", q, err)
	}
	snapshot = s.SnapshotConditions([]byte(`{"speed":"slow"}`))
	q, err = s.QuoteWithFacts("m", "", UsageFacts{Tokens: tokens, RequestedAt: at, Conditions: snapshot})
	if err != nil || q.CostMicroUSD != 1 || q.Reason == reasonMissingFacts {
		t.Fatalf("false condition quote = %+v, %v", q, err)
	}
	q, err = s.QuoteWithFacts("m", "", UsageFacts{Tokens: tokens, RequestedAt: at})
	if err != nil || q.CostMicroUSD != 0 || q.Reason != reasonMissingFacts {
		t.Fatalf("unknown condition quote = %+v, %v", q, err)
	}
}

func TestRequestBodyNotUnknownStaysUnknown(t *testing.T) {
	condition := bodyCondition(t, "/x", JSONString("y"))
	c := Condition{Not: &Condition{RequestBody: &condition}}
	if _, known := evalCondition(c, evalFacts{}); known {
		t.Fatal("NOT of unknown became known")
	}
	knownFalse := map[string]bool{condition.Fingerprint(): false}
	if match, known := evalCondition(c, evalFacts{conditions: knownFalse}); !known || !match {
		t.Fatal("NOT false did not match")
	}
}
