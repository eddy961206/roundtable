package rtmcp

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// verdictLine finds the lines a reviewer or writer uses to state the outcome.
var verdictLine = regexp.MustCompile(`(?i)(판정|결론|verdict)\s*(\([^)\n]*\))?\s*[:：]`)
var fenceLine = regexp.MustCompile("^\\s*(```|~~~)")

// summarizeText keeps, in order, the verdict lines and the marker/signal lines
// ([[...]], WORK:DONE) outside code fences: what decides the next step.
func summarizeText(text string) string {
	var keep []string
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fenceLine.MatchString(line) {
			fenced = !fenced
			continue
		}
		if fenced || trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "[[") || trimmed == "WORK:DONE" || verdictLine.MatchString(trimmed) {
			keep = append(keep, trimmed)
		}
	}
	return strings.Join(keep, "\n")
}

// summarize replaces each seat answer with its summary; chars is the original
// length so the caller can decide whether rt_read is worth it.
func summarize(results []*readResult) {
	for _, r := range results {
		for i := range r.Messages {
			m := &r.Messages[i]
			if m.Author == "user" || m.Kind == "system" {
				continue
			}
			m.Chars = utf8.RuneCountInString(m.Text)
			m.Text = summarizeText(m.Text)
			m.Summarized = true
		}
	}
}

const summaryBudget = 400000 // characters read per ready room before summarizing
