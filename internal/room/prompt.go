package room

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
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
	FirstTurn   bool     // no resumable session: the whole transcript is included
	Forked      bool     // the first turn continues a copy of a base seat's session from another room
	Briefed     bool     // the resumed session already holds the current standing brief
	Part        *Part    // the seat's working/fixing part (D9)
	BlindFirst  bool     // the discussion started with a blind round 1
	Decisions   []string // active decision texts, newest first (standing brief)
	Scratch     string   // server scratch directory named in the standing brief
	HandoffFrom int64    // first turn after a handoff: seq of the summary the history starts at
}

var signalRe = regexp.MustCompile(`^\[\[DISCUSSION:(CONTINUE|AGREE|PASS|CONCLUDE)\]\]$`)

// trailingSignals is shared by all completion consumers. Only consecutive
// signal lines at the end count; ordinary text (including a blank line) ends
// the block. Each consumer removes only its own signals.
func trailingSignals(text string) (lines []string, start int) {
	lines = strings.Split(strings.TrimSpace(text), "\n")
	start = len(lines)
	for start > 0 {
		line := strings.TrimSpace(lines[start-1])
		if line != workDoneSignal && !signalRe.MatchString(line) && !resultMarker.MatchString(line) {
			break
		}
		start--
	}
	return lines, start
}

// ParseSignal extracts the discussion marker and returns the cleaned text.
func ParseSignal(text string) (clean, signal string) {
	lines, start := trailingSignals(text)
	var kept []string
	for i, line := range lines {
		if m := signalRe.FindStringSubmatch(strings.TrimSpace(line)); i >= start && m != nil {
			signal = m[1]
		} else {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), signal
}

func speaker(m *Message, r *Room) string {
	switch m.Kind {
	case "user":
		if m.Forward != nil {
			return "User (" + forwardLabel(m.Forward) + ")"
		}
		return "User"
	case "system":
		return "System"
	}
	if p := r.participant(m.Author); p != nil {
		return "@" + m.Author + " (" + p.Label + ")"
	}
	return "@" + m.Author
}

func oneLineRole(role string) string {
	return strings.Join(strings.Fields(role), " ")
}

// standingBrief is the part of a prompt that only changes with room settings
// and active decisions. A resumed session that already holds the same brief
// gets a one-line reminder; a changed decision changes the key and re-sends it.
func standingBrief(r *Room, me *Participant, decisions []string, scratch string) string {
	var b strings.Builder
	others := make([]string, 0)
	otherRoles := make([]string, 0)
	providerCounts := make(map[string]int)
	providerOrder := make([]string, 0)
	for _, p := range r.enabled() {
		if prov := strings.ToLower(strings.TrimSpace(p.Provider)); prov != "" {
			if providerCounts[prov] == 0 {
				providerOrder = append(providerOrder, prov)
			}
			providerCounts[prov]++
		}
		if p.ID != me.ID {
			others = append(others, "@"+p.ID+"("+p.Label+")")
			if role := oneLineRole(p.Role); role != "" {
				otherRoles = append(otherRoles, fmt.Sprintf("- @%s(%s): %s", p.ID, p.Label, role))
			}
		}
	}
	groupProviders := make([]string, 0)
	for _, prov := range providerOrder {
		if providerCounts[prov] >= 2 {
			groupProviders = append(groupProviders, "@"+prov)
		}
	}
	fmt.Fprintf(&b, "당신은 사용자와 여러 AI 코딩 에이전트가 함께 있는 단톡방 \"%s\"의 참가자 \"@%s\"(%s)입니다.\n", r.Title, me.ID, me.Label)
	fmt.Fprintf(&b, "당신의 공급자는 %s이고 모델은 %s입니다. 같은 공급자나 모델의 다른 참가자도 각자 독립된 세션을 사용합니다.\n", me.Provider, me.Model)
	fmt.Fprintf(&b, "참가자: 사용자(User), %s\n", strings.Join(append([]string{"@" + me.ID + "(" + me.Label + ", 당신)"}, others...), ", "))
	if r.Note != "" {
		fmt.Fprintf(&b, "\n방 공지(항상 유효):\n%s\n", strings.TrimSpace(r.Note))
	}
	if myRole := strings.TrimSpace(me.Role); myRole != "" {
		fmt.Fprintf(&b, "\n당신의 역할:\n%s\n강점은 먼저 깊게 볼 우선순위일 뿐 다른 관점의 지적을 막지 않습니다.\n", myRole)
	}
	if len(otherRoles) > 0 {
		fmt.Fprintf(&b, "\n다른 참가자들의 역할:\n%s\n", strings.Join(otherRoles, "\n"))
	}
	if len(decisions) > 0 {
		b.WriteString("\n현재 결정(최신순):\n")
		for _, text := range decisions {
			fmt.Fprintf(&b, "- %s\n", text)
		}
	}
	b.WriteString("\n규칙:\n")
	b.WriteString("- 아래 대화의 마지막 메시지에 이어 자연스럽게 답하세요. 출력 전체가 채팅 메시지 하나로 그대로 전송됩니다. \"[@" + me.ID + "]\" 같은 접두어나 서명을 붙이지 마세요.\n")
	if len(others) > 0 {
		fmt.Fprintf(&b, "- 다른 참가자를 호출하려면 @이름(%s, @all)을 쓰세요. 그러면 그 참가자가 이어서 답합니다. 호출 없이 언급만 할 때는 @ 없이 이름만 쓰세요. 호출은 꼭 필요할 때만 하세요.\n", strings.Join(others, ", "))
		if len(groupProviders) > 0 {
			fmt.Fprintf(&b, "- %s는 해당 공급자의 모든 활성 참가자를 호출합니다. 한 명만 부르려면 위 목록의 고유 @ID를 쓰세요.\n", strings.Join(groupProviders, ", "))
		}
	}
	b.WriteString("- 공백 없는 이름만 @이름으로 부를 수 있고, 확실하게는 @좌석ID를 쓰세요. 요청 대상도 같은 규칙입니다.\n")
	b.WriteString("- 다른 참가자의 주장은 근거를 확인하고, 동의하지 않으면 이유를 들어 반박하세요. 확인하지 않은 것을 확인했다고 말하지 마세요. 의견과 사실을 구분하세요.\n")
	if r.Persona && len(others) > 0 {
		b.WriteString("- 다른 AI 참가자와는 친한 동료이자 은근한 경쟁자처럼 대화하세요. 좋은 점은 인정하고 빈틈은 재치 있게 짚되, 정확성과 사용자의 목표를 항상 우선하세요.\n")
	}
	switch r.Mode {
	case agent.ModeRead:
		fmt.Fprintf(&b, "- 작업 폴더 %s 의 파일을 읽고 검색할 수 있지만 수정하거나 상태를 바꾸는 명령은 실행하지 마세요.\n", r.Workspace)
	case agent.ModeWrite:
		fmt.Fprintf(&b, "- 작업 폴더 %s 의 파일을 읽고 수정할 수 있습니다. 요청 범위를 벗어난 변경은 하지 마세요. 무엇을 바꿨는지 답변에 적으세요.\n", r.Workspace)
		b.WriteString("- UI·화면을 바꿨다면 직접 화면을 캡처해 [[ATTACH:캡처.png]]로 공유하거나, 이미지 판독·시각 검토에 강점이 있는 좌석에 [[REQUEST:@좌석ID]]로 시각 검토를 요청하세요.\n")
	default:
		b.WriteString("- 도구 실행이나 파일 수정 없이 대화로만 답하세요.\n")
	}
	if rule := waitRule[me.Provider]; rule != "" && r.Mode != agent.ModeChat {
		b.WriteString(rule)
	}
	b.WriteString("- 대화에서 쓰인 언어로 답하세요. 채팅에 어울리게 간결하되, 판단에 필요한 근거는 생략하지 마세요.\n")
	b.WriteString(markerBrief(r.Mode, scratch))
	return b.String()
}

// waitRule keeps a seat from polling a running command once a second: every
// poll is a model call that re-reads the whole context (2026-10-28 A/B,
// docs/토큰효율_측정_20261028.md). Commands that finish sooner still return
// at once, so a longer wait costs no time.
var waitRule = map[string]string{
	"codex": "- 시험·빌드처럼 오래 걸릴 수 있는 명령은 exec_command의 yield_time_ms를 30000으로 주세요(먼저 끝나면 바로 돌아옵니다). 그래도 실행 중이면 빈 write_stdin에 yield_time_ms 300000을 줘서 끝날 때까지 한 번에 기다리세요. 1초 간격으로 반복 확인하지 마세요.\n",
}

// forkMovedNote leads a forked first turn whose CLI directory differs from the base room's.
func forkMovedNote(base, current string) string {
	name := func(dir string) string {
		if dir == "" {
			return "채팅 모드(작업 폴더 없음)"
		}
		return dir
	}
	return fmt.Sprintf("(기반 세션의 작업 폴더는 %s였고, 지금 작업 폴더는 %s입니다. 기반 세션에서 읽은 파일 경로는 지금 작업 폴더 기준으로 다시 해석하고, 내용이 다를 수 있는 파일은 다시 확인하세요.)\n", name(base), name(current))
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
		if role := oneLineRole(me.Role); role != "" {
			fmt.Fprintf(&b, "(당신의 역할: %s — 강점은 먼저 깊게 볼 우선순위일 뿐 다른 관점의 지적을 막지 않습니다.)\n", role)
		}
		if tc.Reason == "discussion" || tc.Reason == "part" || tc.Part != nil {
			b.WriteString("(응답 끝 신호 블록: 요청 결과 [[REQUEST_DONE:요청ID]]/[[REQUEST_REJECT:요청ID]] 이유, 토론 [[DISCUSSION:...]], 파트 완료 WORK:DONE 중 이번 턴에 해당하는 신호만 응답 맨 끝에 빈 줄 없이 한 줄씩 모으세요.)\n")
		}
	} else {
		b.WriteString(standingBrief(r, me, tc.Decisions, tc.Scratch))
	}
	if tc.Forked {
		b.WriteString("\n이 세션은 다른 방의 기반 세션을 복사해 시작했습니다. 그 세션에서 읽고 확인한 내용은 다시 읽지 말고 활용하세요. 다만 그 방의 이름·참가자·역할·규칙·요청은 더 이상 유효하지 않고, 위 안내와 아래 대화가 지금 방의 것입니다. 기반 세션 뒤에 바뀌었을 수 있는 파일은 필요할 때 다시 확인하세요.\n")
	}
	if tc.Parallel || tc.Reason == "discussion" || tc.Part != nil {
		b.WriteString("\n이번 턴:\n")
	}
	if tc.Part != nil {
		fmt.Fprintf(&b, "- 지금은 작업 계획의 파트 %s(%s)를 맡고 있습니다. branch %s 에만 커밋하세요. 파트를 끝냈으면 모든 변경을 커밋한 뒤 응답 끝 신호 블록에 %s 한 줄을 쓰세요.\n", tc.Part.ID, tc.Part.Title, tc.Part.Branch, workDoneSignal)
		if role := oneLineRole(me.Role); role != "" {
			fmt.Fprintf(&b, "- 당신의 역할: %s (강점은 먼저 깊게 볼 우선순위일 뿐 다른 관점의 지적을 막지 않습니다.)\n", role)
		}
	}
	if tc.Parallel {
		b.WriteString("- 같은 메시지를 다른 참가자들도 동시에 받았습니다. 다른 답변을 기다리지 말고 독립적으로 답하세요.\n")
	}
	if tc.Reason == "discussion" {
		if len(r.enabled()) > 1 {
			b.WriteString("- 이번 턴에는 다른 참가자를 추가로 호출할 수 없습니다. 언급은 @ 없이 이름만 쓰세요.\n")
		}
		switch {
		case tc.BlindFirst && tc.Round == 1:
			fmt.Fprintf(&b, "- 지금은 자율 토론 %d/%d 라운드이고, 1라운드는 블라인드입니다. 모든 참가자가 토론 시작 시점까지의 대화만 보고 동시에 답하며, 서로의 1라운드 답은 2라운드에 공개됩니다. 다른 참가자의 의견을 짐작해 맞추지 말고 독립적으로 판단하세요.\n", tc.Round, tc.TotalRounds)
		case tc.BlindFirst && tc.Round == 2:
			fmt.Fprintf(&b, "- 지금은 자율 토론 %d/%d 라운드입니다. 1라운드 답들이 공개됐습니다. 합의된 점은 반복하지 말고, 갈린 쟁점과 새 근거에만 답하세요.\n", tc.Round, tc.TotalRounds)
		default:
			fmt.Fprintf(&b, "- 지금은 자율 토론 %d/%d 라운드입니다. 앞선 답변을 검토해 새 근거가 있을 때만 기여하고, 이미 나온 말은 반복하지 마세요.\n", tc.Round, tc.TotalRounds)
		}
		b.WriteString("- 응답 끝 신호 블록에 다음 중 하나를 한 줄로 붙이세요: [[DISCUSSION:CONTINUE]] [[DISCUSSION:AGREE]] [[DISCUSSION:PASS]] [[DISCUSSION:CONCLUDE]]\n")
		b.WriteString("  새 기여가 있으면 CONTINUE, 새 내용 없이 동의하면 AGREE, 할 말이 없으면 PASS, 충분한 최종 결론을 제시했으면 CONCLUDE.\n")
		b.WriteString("- CONCLUDE나 AGREE를 낼 때는 신호 블록 위에 \"확인한 근거: …\"와 \"확인하지 못한 주장: …\"을 한 줄씩 쓰세요. AGREE는 의견이 같다는 뜻일 뿐 검증이 아닙니다.\n")
	}
	if tc.HandoffFrom > 0 {
		fmt.Fprintf(&b, "\n이 세션은 이전 세션이 남긴 인계 요약(#%d)부터 이어집니다. 그 앞의 대화 원문은 방 기록에 남아 있습니다. 요약에 없는 사실은 짐작하지 말고 파일·기록을 다시 확인하세요.\n", tc.HandoffFrom)
		// The summary is kept out of the history so a long backlog cannot trim it.
		for i, m := range history {
			if m.Seq == tc.HandoffFrom {
				fmt.Fprintf(&b, "\n=== 인계 요약 #%d ===\n%s\n=== 인계 요약 끝 ===\n", m.Seq, strings.TrimSpace(m.Text))
				history = slices.Delete(slices.Clone(history), i, i+1)
				break
			}
		}
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

// BuildPartPrompt starts a part session from the plan alone; room history is never included (D9).
func BuildPartPrompt(plan *WorkPlan, part *Part, role, scratch, provider string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "작업 계획의 파트 %s \"%s\"를 맡았습니다.\n\n", part.ID, part.Title)
	if r := oneLineRole(role); r != "" {
		fmt.Fprintf(&b, "당신의 역할: %s (강점은 먼저 깊게 볼 우선순위일 뿐 다른 관점의 지적을 막지 않습니다.)\n\n", r)
	}
	fmt.Fprintf(&b, "전체 목표:\n%s\n\n", plan.Goal)
	fmt.Fprintf(&b, "완료 조건:\n%s\n\n", part.Brief)
	fmt.Fprintf(&b, "범위: %s\n", strings.Join(part.Scope, ", "))
	fmt.Fprintf(&b, "작업 폴더: %s (branch %s, 기준 커밋 %s)\n\n", part.Worktree, part.Branch, plan.BaseSHA)
	b.WriteString("규칙:\n")
	b.WriteString("- 이 작업 폴더 안에서만 수정하고, 범위 밖의 파일은 바꾸지 마세요.\n")
	fmt.Fprintf(&b, "- 변경은 branch %s 에 커밋하세요. 커밋하지 않은 변경이나 추적하지 않는 파일이 남아 있으면 제출이 거부됩니다.\n", part.Branch)
	fmt.Fprintf(&b, "- 파트를 끝냈으면 응답 끝 신호 블록에 %s 한 줄을 쓰세요. 그러면 다른 공급사 참가자가 diff를 리뷰합니다.\n", workDoneSignal)
	b.WriteString("- 무엇을 바꿨는지 답변에 적으세요.\n")
	b.WriteString(waitRule[provider])
	b.WriteString("- UI·화면을 바꾸는 작업이라면 직접 화면을 캡처해 [[ATTACH:캡처.png]]로 올리거나, 이미지 판독·시각 검토에 강점이 있는 좌석에 [[REQUEST:@좌석ID]]로 시각 검토를 요청하세요.\n")
	b.WriteString(markerBrief(agent.ModeWrite, scratch))
	return b.String()
}

// BuildFixPrompt asks the author to fix the review items it accepted.
func BuildFixPrompt(part *Part) string {
	var b strings.Builder
	fmt.Fprintf(&b, "파트 %s 리뷰의 수용 항목과 남은 이견 항목을 고치세요. 고친 뒤 branch %s 에 커밋하고 응답 끝 신호 블록에 %s 한 줄을 쓰세요. 새 커밋으로 다시 리뷰합니다.\n", part.ID, part.Branch, workDoneSignal)
	for _, round := range part.Review.Rounds {
		for _, item := range round.Items {
			if item.Status != "accepted_pending" && item.Status != "disputed" {
				continue
			}
			f := item.Finding
			fmt.Fprintf(&b, "- %s [%s] %s:%s:%d — %s\n", f.ID, f.Severity, f.Side, f.File, f.Line, f.Scenario)
		}
	}
	return b.String()
}

// BuildVerifyFixPrompt carries the failed check's bounded output to the author.
func BuildVerifyFixPrompt(part *Part, block *Block) string {
	var b strings.Builder
	fmt.Fprintf(&b, "파트 %s 병합 전 검증이 실패했습니다. 파트 worktree에서 원인을 고치세요.\n실패: %s\n", part.ID, block.Message)
	if result := block.Result; result != nil {
		fmt.Fprintf(&b, "검증: %s\n명령 argv: %q\n종료 코드: %d\n시간 초과: %t\n[검증 출력 끝부분]\n%s\n[검증 출력 끝]\n", result.Name, result.Argv, result.ExitCode, result.TimedOut, result.Output)
	}
	b.WriteString(BuildFixPrompt(part))
	return b.String()
}

// ParseWorkDone strips WORK:DONE lines from the terminal signal block.
func ParseWorkDone(text string) (string, bool) {
	lines, start := trailingSignals(text)
	var kept []string
	done := false
	for i, line := range lines {
		if i >= start && strings.TrimSpace(line) == workDoneSignal {
			done = true
		} else {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), done
}

func renderHistory(r *Room, history []*Message) string {
	if len(history) > maxHistoryMessages {
		history = history[len(history)-maxHistoryMessages:]
	}
	var lines []string
	total := 0
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		line := fmt.Sprintf("[#%d %s] %s", m.Seq, speaker(m, r), strings.TrimSpace(m.Text))
		if m.Status == "error" && !m.MarkersApplied {
			line = fmt.Sprintf("[#%d %s] (응답 실패: %s)", m.Seq, speaker(m, r), m.Error)
		} else if m.Status == "error" {
			line = fmt.Sprintf("[#%d %s] (CLI 오류 뒤 완료로 확인된 답) %s", m.Seq, speaker(m, r), strings.TrimSpace(m.Text))
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
