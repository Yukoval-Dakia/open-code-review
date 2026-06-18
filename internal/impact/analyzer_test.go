// internal/impact/analyzer_test.go
package impact

import "testing"

func TestChangedNewLines(t *testing.T) {
	diff := "" +
		"@@ -1,2 +1,3 @@\n" +
		" context\n" + // new line 1 (context)
		"+added a\n" + // new line 2 (added)
		"+added b\n" + // new line 3 (added)
		"@@ -10,1 +11,1 @@\n" +
		"-removed\n" + // not a new line
		"+changed\n" // new line 11 (added)
	got := ChangedNewLines(diff)
	for _, ln := range []int{2, 3, 11} {
		if !got[ln] {
			t.Errorf("line %d should be marked changed; got %v", ln, got)
		}
	}
	if got[1] { // context line is not "changed"
		t.Errorf("context line 1 should not be marked changed")
	}
}
