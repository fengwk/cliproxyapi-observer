//go:build integration

package integration

import "testing"

// Preserve test identity in JSON output while mapping only filesystem names.
// In particular, query IDs must not make Actions discard the entire report.
func TestHarnessArtifactNamesArePortable(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"TestSimple", "TestSimple"},
		{"TestKey-reset_2", "TestKey-reset_2"},
		{"TestManagementRoutesRequireAuthentication//requests/detail?id=nonexistent", "TestManagementRoutesRequireAuthentication__requests_detail_id_nonexistent"},
		{"TestUnsafe/:*?\"<>|\\", "TestUnsafe_________"},
		{"TestWhitespace/ \t\n", "TestWhitespace____"},
		{"TestUnicode/测试", "TestUnicode___"},
		{"TestTraversal/../.", "TestTraversal_____"},
	} {
		if got := artifactTestName(tc.input); got != tc.want {
			t.Errorf("artifactTestName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
