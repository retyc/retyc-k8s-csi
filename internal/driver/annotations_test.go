package driver

import (
	"slices"
	"testing"
)

// local-file: patterns, or "none" alone, as the chart's ValidatingAdmissionPolicy accepts them.
func TestParseMountSettingsLocalFile(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string // nil: refused
	}{
		{"*.tmp  .~lock*", []string{"*.tmp", ".~lock*"}},
		{"none", []string{"none"}},
		{" none ", []string{"none"}},
		{"nonesuch *.tmp", []string{"nonesuch", "*.tmp"}},
		{"none *.tmp", nil},
		{"*.tmp none", nil},
		{"none none", nil},
	} {
		ms, err := parseMountSettings(map[string]string{AnnotationUID: "1000", AnnotationLocalFile: tc.raw})
		switch {
		case tc.want == nil && err == nil:
			t.Errorf("local-file %q accepted as %q", tc.raw, ms.Options.LocalFile)
		case tc.want != nil && err != nil:
			t.Errorf("local-file %q: %v", tc.raw, err)
		case tc.want != nil && !slices.Equal(ms.Options.LocalFile, tc.want):
			t.Errorf("local-file %q = %q, want %q", tc.raw, ms.Options.LocalFile, tc.want)
		}
	}
}
