package release

import "testing"

func TestEnforceTrustedComment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		got     string
		wantTag string
		wantErr bool
	}{
		{"exact match", "mtroamd v1.1.3", "v1.1.3", false},
		{"trailing whitespace tolerated", "mtroamd v1.1.3\n", "v1.1.3", false},
		{"leading whitespace tolerated", "  mtroamd v1.1.3", "v1.1.3", false},
		{"different tag rejected", "mtroamd v1.1.2", "v1.1.3", true},
		{"older tag rejected (rollback attack)", "mtroamd v0.9.0", "v1.1.3", true},
		{"no prefix rejected", "v1.1.3", "v1.1.3", true},
		{"different repo rejected", "other-tool v1.1.3", "v1.1.3", true},
		{"empty comment rejected", "", "v1.1.3", true},
		{"case-sensitive (uppercase V)", "mtroamd V1.1.3", "v1.1.3", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := EnforceTrustedComment(tc.got, tc.wantTag)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for got=%q want=%q", tc.got, tc.wantTag)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
