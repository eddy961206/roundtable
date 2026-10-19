package room

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"roundtable/internal/agent"
)

const (
	maxHistoryMessages = 40
	maxHistoryChars    = 60 * 1024
)

// turnContext tells the prompt builder why the agent is speaking.
type turnContext struct {
	Reason      string // mention | broadcast | discussion
	Parallel    bool   // broadcast delivered to several agents at once
	Round       int
	TotalRounds int
	FirstTurn   bool // no resumable session: the whole transcript is included
	Briefed     bool // the resumed session already holds the current standing brief
}

var signalRe = regexp.MustCompile(`\[\[DISCUSSION:(CONTINUE|AGREE|PASS|CONCLUDE)\]\]\s*$`)

// ParseSignal extracts the discussion marker and returns the cleaned text.
func ParseSignal(text string) (clean, signal string) {
	m := signalRe.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return strings.TrimSpace(text), ""
	}
	clean = strings.TrimSpace(signalRe.ReplaceAllString(strings.TrimSpace(text), ""))
	return clean, m[1]
}

func speaker(m *Message, r *Room) string {
	switch m.Kind {
	case "user":
		return "User"
	case "system":
		return "System"
	}
	if p := r.participant(m.Author); p != nil {
		return "@" + m.Author + " (" + p.Label + ")"
	}
	return "@" + m.Author
}

// standingBrief is the part of a prompt that only changes with room settings.
// A resumed session that already holds the same brief gets a one-line reminder.
func standingBrief(r *Room, me *Participant) string {
	var b strings.Builder
	others := make([]string, 0)
	for _, p := range r.enabled() {
		if p.ID != me.ID {
			others = append(others, "@"+p.ID+"("+p.Label+")")
		}
	}
	fmt.Fprintf(&b, "당신은 사용자와 여러 AI 코딩 에이전트가 함께 있는 단톡방 \"%s\"의 참가자 \"@%s\"(%s)입니다.\n", r.Title, me.ID, me.Label)
	fmt.Fprintf(&b, "당신의 공급자는 %s이고 모델은 %s입니다. 같은 공급자나 모델의 다른 참가자도 각자 독립된 세션을 사용합니다.\n", me.Provider, me.Model)
	fmt.Fprintf(&b, "참가자: 사용자(User), %s\n", strings.Join(append([]string{"@" + me.ID + "(" + me.Label + ", 당신)"}, others...), ", "))
	if r.Note != "" {
		fmt.Fprintf(&b, "\n방 공지(항상 유효):\n%s\n", strings.TrimSpace(r.Note))
	}
	b.WriteString("\n규칙:\n")
	b.WriteString("- 아래 대화의 마지막 메시지에 이어 자연스럽게 답하세요. 출력 전체가 채팅 메시지 하나로 그대로 전송됩니다. \"[@" + me.ID + "]\" 같은 접두어나 서명을 붙이지 마세요.\n")
	if len(others) > 0 {
		fmt.Fprintf(&b, "- 다른 참가자를 호출하려면 @이름(%s, @all)을 쓰세요. 그러면 그 참가자가 이어서 답합니다. 호출 없이 언급만 할 때는 @ 없이 이름만 쓰세요. 호출은 꼭 필요할 때만 하세요.\n", strings.Join(others, ", "))
		b.WriteString("- @claude, @codex, @agy는 해당 공급자의 모든 활성 참가자를 호출합니다. 한 명만 부르려면 위 목록의 고유 @ID를 쓰세요.\n")
	}
	b.WriteString("- 다른 참가자의 주장은 근거를 확인하고, 동의하지 않으면 이유를 들어 반박하세요. 확인하지 않은 것을 확인했다고 말하지 마세요. 의견과 사실을 구분하세요.\n")
	if r.Persona && len(others) > 0 {
		b.WriteString("- 다른 AI 참가자와는 친한 동료이자 은근한 경쟁자처럼 대화하세요. 좋은 점은 인정하고 빈틈은 재치 있게 짚되, 정확성과 사용자의 목표를 항상 우선하세요.\n")
	}
	switch r.Mode {
	case agent.ModeRead:
		fmt.Fprintf(&b, "- 작업 폴더 %s 의 파일을 읽고 검색할 수 있지만 수정하거나 상태를 바꾸는 명령은 실행하지 마세요.\n", r.Workspace)
	case agent.ModeWrite:
		fmt.Fprintf(&b, "- 작업 폴더 %s 의 파일을 읽고 수정할 수 있습니다. 요청 범위를 벗어난 변경은 하지 마세요. 무엇을 바꿨는지 답변에 적으세요.\n", r.Workspace)
	default:
		b.WriteString("- 도구 실행이나 파일 수정 없이 대화로만 답하세요.\n")
	}
	b.WriteString("- 대화에서 쓰인 언어로 답하세요. 채팅에 어울리게 간결하되, 판단에 필요한 근거는 생략하지 마세요.\n")
	return b.String()
}

// briefKey identifies a standing brief so an unchanged one is not re-sent.
func briefKey(brief string) string {
	sum := sha256.Sum256([]byte(brief))
	return hex.EncodeToString(sum[:8])
}

// BuildPrompt renders the instruction block plus the messages the agent has not seen.
func BuildPrompt(r *Room, me *Participant, history []*Message, tc turnContext) string {
	var b strings.Builder
	if tc.Briefed {
		fmt.Fprintf(&b, "(단톡방 \"%s\"의 참가자·공지·규칙은 이 세션에서 받은 그대로입니다. 당신은 \"@%s\"(%s)입니다.)\n", r.Title, me.ID, me.Label)
	} else {
		b.WriteString(standingBrief(r, me))
	}
	if tc.Parallel || tc.Reason == "discussion" {
		b.WriteString("\n이번 턴:\n")
	}
	if tc.Parallel {
		b.WriteString("- 같은 메시지를 다른 참가자들도 동시에 받았습니다. 다른 답변을 기다리지 말고 독립적으로 답하세요.\n")
	}
	if tc.Reason == "discussion" {
		if len(r.enabled()) > 1 {
			b.WriteString("- 이번 턴에는 다른 참가자를 추가로 호출할 수 없습니다. 언급은 @ 없이 이름만 쓰세요.\n")
		}
		fmt.Fprintf(&b, "- 지금은 자율 토론 %d/%d 라운드입니다. 앞선 답변을 검토해 새 근거가 있을 때만 기여하고, 이미 나온 말은 반복하지 마세요.\n", tc.Round, tc.TotalRounds)
		b.WriteString("- 응답 마지막 줄에 반드시 다음 중 하나만 붙이세요: [[DISCUSSION:CONTINUE]] [[DISCUSSION:AGREE]] [[DISCUSSION:PASS]] [[DISCUSSION:CONCLUDE]]\n")
		b.WriteString("  새 기여가 있으면 CONTINUE, 새 내용 없이 동의하면 AGREE, 할 말이 없으면 PASS, 충분한 최종 결론을 제시했으면 CONCLUDE.\n")
	}
	if tc.FirstTurn {
		b.WriteString("\n=== 지금까지의 대화 ===\n")
	} else {
		b.WriteString("\n=== 당신의 마지막 발언 이후 새로 올라온 메시지 ===\n")
	}
	b.WriteString(renderHistory(r, history))
	b.WriteString("=== 끝 ===\n\n")
	fmt.Fprintf(&b, "지금 \"@%s\"로서 답할 차례입니다.", me.ID)
	return b.String()
}

func renderHistory(r *Room, history []*Message) string {
	if len(history) > maxHistoryMessages {
		history = history[len(history)-maxHistoryMessages:]
	}
	var lines []string
	total := 0
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		line := fmt.Sprintf("[%s] %s", speaker(m, r), strings.TrimSpace(m.Text))
		if m.Status == "error" {
			line = fmt.Sprintf("[%s] (응답 실패: %s)", speaker(m, r), m.Error)
		}
		total += len(line)
		if total > maxHistoryChars {
			if i == len(history)-1 { // the newest message always goes through, trimmed
				lines = append(lines, clipRunes(line, maxHistoryChars)+"\n…[메시지가 너무 길어 뒷부분 생략]")
				continue
			}
			lines = append(lines, "(이전 메시지 생략)")
			break
		}
		lines = append(lines, line)
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	if len(lines) == 0 {
		return "(없음)\n"
	}
	return strings.Join(lines, "\n") + "\n"
}

func clipRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
