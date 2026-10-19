package room

import (
	"regexp"
	"strings"
	"unicode"
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

// ParseParticipantMentions accepts stable seat IDs, provider-wide groups and
// display labels ("@Astra"); a label shared by several seats calls all of them.
// Provider aliases expand even when a legacy seat has the same ID as its provider.
// IDs and providers take precedence over labels. Labels cannot contain whitespace
// and accept only the explicit Korean particles in labelMatches.
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
		identityMatched := false
		for _, p := range participants {
			if tokenMatches(token, strings.ToLower(p.ID)) || (p.Provider != "" && tokenMatches(token, strings.ToLower(p.Provider))) {
				identityMatched = true
				add(p.ID)
			}
		}
		if identityMatched {
			continue
		}
		for _, p := range participants {
			if labelMatches(token, strings.ToLower(p.Label)) {
				add(p.ID)
			}
		}
	}
	return out
}

func labelMatches(token, label string) bool {
	if label == "" || strings.IndexFunc(label, unicode.IsSpace) >= 0 || !strings.HasPrefix(token, label) {
		return false
	}
	switch token[len(label):] {
	case "", "야", "아", "님", "에게", "한테", "께", "이", "가", "은", "는", "을", "를", "도", "의", "와", "과", "랑", "이랑":
		return true
	}
	return false
}

// Messages written before mention snapshots were introduced have no field.
func (m *Message) mentionIDs(participants []*Participant) []string {
	if m.Mentions != nil {
		return *m.Mentions
	}
	return ParseParticipantMentions(ordinaryMentionText(m.Text), participants)
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
