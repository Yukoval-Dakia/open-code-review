// internal/impact/analyzer.go
package impact

import (
	"regexp"
	"strconv"
	"strings"
)

// Symbol is a definition found in a changed file.
type Symbol struct {
	Name    string
	Kind    string // function | method | class | interface | type | enum | const | export
	DefLine int
}

// Reference is a confirmed use of a symbol in another file.
type Reference struct {
	File string
	Line int
	Kind string // call | import | type-use | ref
}

// LangAnalyzer parses one language's definitions and references.
type LangAnalyzer interface {
	Supports(path string) bool
	// ChangedSymbols returns definitions whose line intersects changed.
	ChangedSymbols(content string, changed map[int]bool) ([]Symbol, error)
	// References returns confirmed references to name in content (path is for kind hints).
	References(path, content, name string) ([]Reference, error)
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// ChangedNewLines parses a unified diff and returns the set of NEW-file line
// numbers that were added (lines starting with '+', excluding the '+++' header).
func ChangedNewLines(diff string) map[int]bool {
	changed := map[int]bool{}
	newLine := 0
	for _, line := range strings.Split(diff, "\n") {
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			newLine, _ = strconv.Atoi(m[1])
			continue
		}
		switch {
		case strings.HasPrefix(line, "+++"):
			// file header, ignore
		case strings.HasPrefix(line, "+"):
			changed[newLine] = true
			newLine++
		case strings.HasPrefix(line, "-"):
			// removed from old file; new-file numbering unaffected
		default:
			newLine++ // context line
		}
	}
	return changed
}
