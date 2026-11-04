package csvx

import "testing"

func TestSafe(t *testing.T) {
	for in, want := range map[string]string{"=HYPERLINK(\"x\")": "'=HYPERLINK(\"x\")", "+251911": "'+251911", "-5": "'-5",
		"@SUM(A1)": "'@SUM(A1)", "Abebe": "Abebe", "": ""} {
		if got := Safe(in); got != want {
			t.Errorf("Safe(%q) = %q, want %q", in, got, want)
		}
	}
}
