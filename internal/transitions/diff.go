package transitions

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// DiffJSON returns a unified diff string comparing two JSON blobs.
// Returns an empty string when the blobs are semantically equal or when
// either is nil/empty.
func DiffJSON(before, after json.RawMessage) (string, error) {
	a := prettyJSON(before)
	b := prettyJSON(after)
	if a == b {
		return "", nil
	}
	return difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:       difflib.SplitLines(a),
		B:       difflib.SplitLines(b),
		Context: 3,
	})
}

// ColorizeDiff adds ANSI color codes to a unified diff string.
// Lines starting with "-" are red; lines starting with "+" are green.
func ColorizeDiff(in string) string {
	const (
		red   = "\x1b[31m"
		green = "\x1b[32m"
		reset = "\x1b[0m"
	)
	var out strings.Builder
	for _, line := range difflib.SplitLines(in) {
		switch {
		case strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++"):
			out.WriteString(line)
		case strings.HasPrefix(line, "-"):
			out.WriteString(red + line + reset)
		case strings.HasPrefix(line, "+"):
			out.WriteString(green + line + reset)
		default:
			out.WriteString(line)
		}
	}
	return out.String()
}

func prettyJSON(body []byte) string {
	if len(body) == 0 {
		return "null\n"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err == nil {
		out.WriteByte('\n')
		return out.String()
	}
	return string(body) + "\n"
}
