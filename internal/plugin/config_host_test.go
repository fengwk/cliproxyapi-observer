package plugin

import "testing"

// The real host always injects priority and may include installation provenance;
// accepting only product fields would make every native registration fail.
func TestHostInjectedConfiguration(t *testing.T) {
	for _, raw := range []string{
		"enabled: true\npriority: 0\ndb: local.db\n",
		"enabled: true\npriority: -1\ndb: local.db\nstore:\n  version: 0.6.0\n  release-tag: v0.6.0\n",
	} {
		c, err := parseConfig([]byte(raw))
		if err != nil || !c.Enabled || c.DB != "local.db" {
			t.Fatalf("host config rejected: %+v %v", c, err)
		}
	}
	for _, raw := range []string{"priority: false", "priority: 1.5", "store: bad", "capture-bodies: true"} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid product/host field accepted: %q", raw)
		}
	}
}
