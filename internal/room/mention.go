package room

import (
	"regexp"
	"strings"
)

var mentionRe = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_@/.-])@([\p{L}\p{N}_-]+)`)
var fenceRe = regexp.MustCompile("(?s)```.*?(?:```|$)")
var inlineCodeRe = regexp.MustCompile("`[^`\n]*`")

// ParseMentions returns the participant ids addressed in text, in order.
// "@all" (or "@모두", "@전원") expands to every id. Korean particles after the
// name are tolerated ("@codex야"), but "@codexx" is a different name.
func ParseMentions(text string, ids []string) []string {
	participants := make([]*Participant, 0, len(ids))
	for _, id := range ids {
		participants = append(participants, &Participant{ID: id})
	}
	return ParseParticipantMentions(text, participants)
}

// ParseParticipantMentions accepts stable seat IDs and provider-wide groups.
// Provider aliases expand even when a legacy seat has the same ID as its provider.
func ParseParticipantMentions(text string, participants []*Participant) []string {
	src := fenceRe.ReplaceAllStringFunc(text, func(s string) string { return strings.Repeat(" ", len(s)) })
	src = inlineCodeRe.ReplaceAllStringFunc(src, func(s string) string { return strings.Repeat(" ", len(s)) })
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, m := range mentionRe.FindAllStringSubmatch(src, -1) {
		token := strings.ToLower(m[1])
		if token == "all" || token == "모두" || token == "전원" || token == "everyone" {
			for _, p := range participants {
				if !p.Removed {
					add(p.ID)
				}
			}
			continue
		}
		for _, p := range participants {
			if tokenMatches(token, strings.ToLower(p.ID)) || (p.Provider != "" && tokenMatches(token, strings.ToLower(p.Provider))) {
				add(p.ID)
			}
		}
	}
	return out
}

func tokenMatches(token, alias string) bool {
	if token == alias {
		return true
	}
	if !strings.HasPrefix(token, alias) {
		return false
	}
	rest := token[len(alias):]
	if rest == "" {
		return true
	}
	c := rest[0]
	return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-')
}

// HasMention reports whether the text addresses anyone at all (including @all).
func HasMention(text string, ids []string) bool { return len(ParseMentions(text, ids)) > 0 }
