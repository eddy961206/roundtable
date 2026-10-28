package rtmcp

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// verdictLine finds the lines a reviewer or writer uses to state the outcome.
var verdictLine = regexp.MustCompile(`(?i)(판정|결론|verdict)\s*(\([^)\n]*\))?\s*[:：]`)
var fenceLine = regexp.MustCompile("^\\s*(```|~~~)")

// summaryTail is how many closing prose lines stand in for a missing verdict.
const summaryTail = 3

// summarizeText keeps, in order, the verdict lines and the marker/signal lines
// ([[...]], WORK:DONE) outside code fences: what decides the next step. An
// answer without a verdict line (e.g. a writer reporting progress) keeps its
// last prose lines instead, so the summary is never empty for a real answer.
func summarizeText(text string) string {
	var keep, prose []string
	verdict, fenced := false, false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fenceLine.MatchString(line) {
			fenced = !fenced
			continue
		}
		if fenced || trimmed == "" {
			continue
		}
		switch {
		case verdictLine.MatchString(trimmed):
			verdict = true
			keep = append(keep, trimmed)
		case strings.HasPrefix(trimmed, "[[") || trimmed == "WORK:DONE":
			keep = append(keep, trimmed)
		default:
			prose = append(prose, trimmed)
		}
	}
	if !verdict {
		keep = append(prose[max(0, len(prose)-summaryTail):], keep...)
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
