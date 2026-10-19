package room

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

const findingHeader = "| 번호 | 심각도 | 파일:줄 | 재현 시나리오 | 확신도 |\n| --- | --- | --- | --- | --- |\n"
const findingOne = "| 1 | high | head:src/example.go:42 | 조건에서 잘못된 결과 | high |\n"
const findingTwo = "| 2 | low | base:src/deleted.go:3 | 삭제된 검증이 필요함 | medium |\n"
const authorHeader = "| 번호 | 응답 | 근거 |\n| --- | --- | --- |\n"
const rebuttalHeader = "| 번호 | 판정 | 근거 |\n| --- | --- | --- |\n"
const recheckHeader = "| 번호 | 확인 | 근거 |\n| --- | --- | --- |\n"

func protocolInput(head string) ReviewInput {
	return ReviewInput{
		Ref:  ReviewRef{PartID: "p1", BaseSHA: "base-commit", HeadSHA: head},
		Goal: "입력 검증", Brief: "잘못된 입력 거부", Scope: []string{"src/"},
		Diff: &PartDiff{PartID: "p1", BaseSHA: "base-commit", HeadSHA: head,
			Added: 2, Deleted: 1, Files: []DiffFile{{Path: "src/example.go", Status: "M", Added: 2, Deleted: 1, Patch: "-old\n+new"}}},
	}
}

func startedReview(t *testing.T, max int) *ReviewState {
	t.Helper()
	rs := NewReviewState(max)
	step, err := rs.Start(protocolInput("head-one"))
	if err != nil || step.Who != "reviewer" || step.Kind != "findings" || rs.Rounds[0].Ref.Round != 1 {
		t.Fatalf("start: step=%+v state=%+v err=%v", step, rs, err)
	}
	return rs
}

func TestReviewCorrectionAllowancePerStepAndRound(t *testing.T) {
	rs := startedReview(t, 1)
	correct := func(reply, kind, outcome string) {
		t.Helper()
		_, _, err := rs.Apply("bad table")
		if err == nil || rs.RetryFormat(err) == nil || rs.Pending.FormatRetries != 1 || rs.RetryFormat(err) != nil {
			t.Fatal("expected exactly one correction for this step")
		}
		applyReview(t, rs, reply, kind, outcome)
	}
	correct(findingHeader+findingOne+findingTwo, "author_reply", "")
	correct(authorHeader+"| r1-1 | ACCEPT | fix |\n| r1-2 | REJECT | disagree |\n", "rebuttal", "")
	correct(rebuttalHeader+"| r1-2 | MAINTAIN | still broken |\n", "", "disputed")
	if err := rs.AllowExtraRound(); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Resubmit(protocolInput("head-two")); err != nil {
		t.Fatal(err)
	}
	correct(recheckHeader+"| r1-1 | FIXED | verified |\n| r1-2 | UNFIXED | still broken |\n"+findingHeader, "", "disputed")
	if rs.Rounds[0].Items[1].Status != "disputed" || rs.Summary.Unresolved != 1 || rs.Summary.Passed {
		t.Fatal("UNFIXED silently waived the disputed finding")
	}
	if err := rs.AllowExtraRound(); err != nil || rs.MaxRounds != 3 {
		t.Fatalf("second human retry must add exactly one round: %v", err)
	}
	if _, err := rs.Resubmit(protocolInput("head-three")); err != nil {
		t.Fatal(err)
	}
	correct(recheckHeader+"| r1-2 | FIXED | verified |\n"+findingHeader, "", "passed")
}

func TestReviewRebuttalOnlyCurrentRoundRejections(t *testing.T) {
	for _, action := range []string{"ACCEPT", "REJECT"} {
		t.Run(action, func(t *testing.T) {
			rs := startedReview(t, 3)
			applyReview(t, rs, findingHeader+findingOne, "author_reply", "")
			applyReview(t, rs, authorHeader+"| r1-1 | REJECT | disagree |\n", "rebuttal", "")
			applyReview(t, rs, rebuttalHeader+"| r1-1 | MAINTAIN | still broken |\n", "", "disputed")
			if err := rs.AllowExtraRound(); err != nil {
				t.Fatal(err)
			}
			if _, err := rs.Resubmit(protocolInput("head-two")); err != nil {
				t.Fatal(err)
			}
			applyReview(t, rs, recheckHeader+"| r1-1 | UNFIXED | remains on new SHA |\n"+findingHeader+findingOne, "author_reply", "")
			reply := authorHeader + "| r2-1 | " + action + " | current round evidence |\n"
			if action == "ACCEPT" {
				applyReview(t, rs, reply, "", "disputed")
				if rs.Rounds[1].Items[0].Status != "accepted_pending" {
					t.Fatal("new acceptance was lost")
				}
			} else {
				applyReview(t, rs, reply, "rebuttal", "")
				if !strings.Contains(rs.Pending.Prompt, "응답할 서버 ID: r2-1\n") {
					t.Fatal("rebuttal prompt includes an already rechecked finding")
				}
				assertReviewErrorUnchanged(t, rs, rebuttalHeader+"| r1-1 | WITHDRAW | old item |\n| r2-1 | WITHDRAW | new item |\n")
				applyReview(t, rs, rebuttalHeader+"| r2-1 | WITHDRAW | new item withdrawn |\n", "", "disputed")
			}
			if old := rs.Rounds[0].Items[0]; old.Status != "disputed" || old.Reviewer.Action != "UNFIXED" {
				t.Fatal("current round rebuttal overwrote the previous finding's recheck")
			}
		})
	}
}

func applyReview(t *testing.T, rs *ReviewState, reply, kind, outcome string) {
	t.Helper()
	next, got, err := rs.Apply(reply)
	if err != nil || got != outcome {
		t.Fatalf("apply: next=%+v outcome=%q want=%q err=%v", next, got, outcome, err)
	}
	if kind == "" {
		if next != nil || rs.Pending != nil || rs.Phase != outcome {
			t.Fatalf("expected terminal step: %+v", rs)
		}
	} else if next == nil || next.Kind != kind || rs.Phase != kind || next.Prompt == "" {
		t.Fatalf("expected next %q, got %+v", kind, next)
	}
}

func assertReviewErrorUnchanged(t *testing.T, rs *ReviewState, reply string) {
	t.Helper()
	before, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	pending := rs.Pending
	next, outcome, err := rs.Apply(reply)
	if err == nil || next != nil || outcome != "" {
		t.Fatalf("expected parse error: next=%+v outcome=%q err=%v", next, outcome, err)
	}
	after, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || rs.Pending != pending {
		t.Fatalf("error changed state:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestReviewZeroFindings(t *testing.T) {
	rs := startedReview(t, 3)
	applyReview(t, rs, findingHeader, "", "passed")
	if rs.Summary != (ReviewSummary{Complete: true, Passed: true}) {
		t.Fatalf("summary: %+v", rs.Summary)
	}
	assertReviewErrorUnchanged(t, rs, findingHeader)
}

func TestReviewFindingsParsing(t *testing.T) {
	rs := startedReview(t, 3)
	reply := findingHeader + findingTwo + strings.Replace(findingOne, "조건에서 잘못된 결과", `조건 A \| B → 결과`, 1)
	applyReview(t, rs, strings.ReplaceAll(reply, "\n", "\r\n"), "author_reply", "")
	items := rs.Rounds[0].Items
	if items[0].Finding.ID != "r1-1" || items[0].Finding.Scenario != "조건 A | B → 결과" || items[1].Finding.Side != "base" || items[1].Finding.Line != 3 {
		t.Fatalf("parsed items: %+v", items)
	}
	if rs.Pending.Who != "author" || rs.Summary != (ReviewSummary{Unresolved: 2}) {
		t.Fatalf("author state: %+v", rs)
	}
}

func TestReviewFindingErrorsAreAtomic(t *testing.T) {
	cases := map[string]string{
		"empty": "", "whitespace": " \n\t", "prose only": "지적 없음",
		"duplicate":         findingHeader + findingOne + findingOne,
		"gap":               findingHeader + findingOne + strings.Replace(findingTwo, "| 2 |", "| 3 |", 1),
		"zero":              findingHeader + strings.Replace(findingOne, "| 1 |", "| 0 |", 1),
		"server id":         findingHeader + strings.Replace(findingOne, "| 1 |", "| r1-1 |", 1),
		"leading zero":      findingHeader + strings.Replace(findingOne, "| 1 |", "| 01 |", 1),
		"severity":          findingHeader + strings.Replace(findingOne, "high", "urgent", 1),
		"confidence":        findingHeader + strings.Replace(findingOne, "| high |\n", "| certain |\n", 1),
		"blank scenario":    findingHeader + strings.Replace(findingOne, "조건에서 잘못된 결과", " ", 1),
		"unescaped pipe":    findingHeader + strings.Replace(findingOne, "조건에서 잘못된 결과", "조건 | 결과", 1),
		"multiline cell":    findingHeader + strings.Replace(findingOne, "조건에서 잘못된 결과", "조건\n결과", 1),
		"wrong header":      strings.Replace(findingHeader, "심각도", "중요도", 1),
		"bad separator":     strings.Replace(findingHeader, "---", "--", 1),
		"unexpected table":  findingHeader + "\n설명\n| 이름 | 값 |\n| --- | --- |\n",
		"wrong stage table": findingHeader + authorHeader,
		"missing separator": strings.Split(findingHeader, "\n")[0],
		"reordered header":  strings.Replace(findingHeader, "심각도 | 파일:줄", "파일:줄 | 심각도", 1),
		"extra table":       findingHeader + findingHeader,
		"no closing pipe":   findingHeader + strings.TrimSuffix(findingOne, "|\n"),
		"no opening pipe":   findingHeader + strings.TrimPrefix(findingOne, "|"),
		"fenced table":      "```\n" + findingHeader + "```",
		"tilde fence table": "~~~\n" + findingHeader + "~~~",
	}
	for _, location := range []string{"src/example.go:42", "other:src/example.go:42", "head:../secret:1", "head:/root/file:1", "head:C:/file:1", `head:src\file:1`, "head:src/../file:1", "head:.:1", "head:src/file:0", "head:src/file:-1", "head:src/file:01", "head:src/file:999999999999999999999999"} {
		cases[location] = findingHeader + strings.Replace(findingOne, "head:src/example.go:42", location, 1)
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) { assertReviewErrorUnchanged(t, startedReview(t, 3), reply) })
	}
}

func TestReviewAcceptFixRecheck(t *testing.T) {
	rs := startedReview(t, 3)
	applyReview(t, rs, findingHeader+findingOne, "author_reply", "")
	applyReview(t, rs, authorHeader+"| r1-1 | ACCEPT | 입력 검증 추가 |\n", "", "fix")
	if rs.Summary != (ReviewSummary{Unresolved: 1}) || rs.Rounds[0].Items[0].Status != "accepted_pending" {
		t.Fatalf("fix summary/items: %+v", rs)
	}
	step, err := rs.Resubmit(protocolInput("head-two"))
	if err != nil || step.Kind != "recheck" || step.Who != "reviewer" || rs.Rounds[1].Ref.Round != 2 {
		t.Fatalf("resubmit: step=%+v err=%v", step, err)
	}
	applyReview(t, rs, recheckHeader+"| r1-1 | FIXED | 새 SHA에서 검증 확인 |\n"+findingHeader, "", "passed")
	item := rs.Rounds[0].Items[0]
	if item.Status != "fixed" || item.Author.Action != "ACCEPT" || item.Reviewer.Action != "FIXED" || rs.Summary != (ReviewSummary{Complete: true, Passed: true}) {
		t.Fatalf("fixed state: %+v", rs)
	}
}

func TestReviewRebuttal(t *testing.T) {
	for _, verdict := range []string{"MAINTAIN", "WITHDRAW"} {
		t.Run(verdict, func(t *testing.T) {
			rs := startedReview(t, 1)
			applyReview(t, rs, findingHeader+findingOne, "author_reply", "")
			applyReview(t, rs, authorHeader+"| r1-1 | REJECT | 호출자가 먼저 검사함 |\n", "rebuttal", "")
			if rs.Pending.Who != "reviewer" || rs.Summary.Unresolved != 1 || rs.Summary.Complete {
				t.Fatalf("rebuttal: %+v", rs)
			}
			outcome, status, unresolved := "disputed", "disputed", 1
			if verdict == "WITHDRAW" {
				outcome, status, unresolved = "passed", "withdrawn", 0
			}
			applyReview(t, rs, rebuttalHeader+"| r1-1 | "+verdict+" | 호출 경로 확인 |\n", "", outcome)
			if len(rs.Rounds) != 1 || rs.Rounds[0].Items[0].Status != status || rs.Summary != (ReviewSummary{Unresolved: unresolved, Complete: true, Passed: outcome == "passed"}) {
				t.Fatalf("verdict state: %+v", rs)
			}
		})
	}
}

func TestReviewActionErrorsAreAtomic(t *testing.T) {
	for _, phase := range []string{"author_reply", "rebuttal", "recheck"} {
		for _, fault := range []string{"empty", "missing", "duplicate", "unknown", "enum", "blank reason", "wrong header", "duplicate table", "unexpected table"} {
			t.Run(phase+"/"+fault, func(t *testing.T) {
				rs := startedReview(t, 3)
				applyReview(t, rs, findingHeader+findingOne+findingTwo, "author_reply", "")
				header, action, suffix := authorHeader, "ACCEPT", ""
				if phase == "rebuttal" {
					applyReview(t, rs, authorHeader+"| r1-1 | REJECT | 기존 검증 |\n| r1-2 | REJECT | 기존 검증 |\n", "rebuttal", "")
					header, action = rebuttalHeader, "WITHDRAW"
				}
				if phase == "recheck" {
					applyReview(t, rs, authorHeader+"| r1-1 | ACCEPT | 검증 |\n| r1-2 | ACCEPT | 검증 |\n", "", "fix")
					if _, err := rs.Resubmit(protocolInput("head-two")); err != nil {
						t.Fatal(err)
					}
					header, action, suffix = recheckHeader, "FIXED", findingHeader
				}
				first := "| r1-1 | " + action + " | 확인 |\n"
				second := "| r1-2 | " + action + " | 확인 |\n"
				reply := header + first + second + suffix
				switch fault {
				case "empty":
					reply = ""
				case "missing":
					reply = header + first + suffix
				case "duplicate":
					reply = header + first + first + suffix
				case "unknown":
					reply = header + first + strings.Replace(second, "r1-2", "r9-2", 1) + suffix
				case "enum":
					reply = header + first + strings.Replace(second, action, "YES", 1) + suffix
				case "blank reason":
					reply = header + first + strings.Replace(second, "확인", "", 1) + suffix
				case "wrong header":
					reply = findingHeader + first + second + suffix
				case "duplicate table":
					reply += "\n설명\n" + header
				case "unexpected table":
					reply += "\n설명\n| 이름 | 값 |\n| --- | --- |\n"
				}
				assertReviewErrorUnchanged(t, rs, reply)
			})
		}
	}
}

func TestReviewRoundLimitAndMixedItems(t *testing.T) {
	rs := startedReview(t, 2)
	applyReview(t, rs, findingHeader+findingOne+findingTwo, "author_reply", "")
	applyReview(t, rs, authorHeader+"| r1-1 | ACCEPT | 검증 수정 |\n| r1-2 | REJECT | 기존 검사 있음 |\n", "rebuttal", "")
	assertReviewErrorUnchanged(t, rs, rebuttalHeader+"| r1-1 | WITHDRAW | 대상 아님 |\n| r1-2 | WITHDRAW | 확인 |\n")
	applyReview(t, rs, rebuttalHeader+"| r1-2 | WITHDRAW | 검사 확인 |\n", "", "fix")
	if _, err := rs.Resubmit(protocolInput("head-two")); err != nil {
		t.Fatal(err)
	}
	applyReview(t, rs, recheckHeader+"| r1-1 | UNFIXED | 아직 재현됨 |\n"+findingHeader+findingOne, "author_reply", "")
	if rs.Rounds[1].Items[0].Finding.ID != "r2-1" || rs.Summary.Unresolved != 2 {
		t.Fatalf("new finding: %+v", rs)
	}
	applyReview(t, rs, authorHeader+"| r2-1 | ACCEPT | 새 항목 수정 필요 |\n", "", "limit")
	if rs.Summary != (ReviewSummary{Unresolved: 2, Complete: true}) {
		t.Fatalf("limit: %+v", rs.Summary)
	}
	before, _ := json.Marshal(rs)
	if _, err := rs.Resubmit(protocolInput("head-three")); err == nil {
		t.Fatal("limit allowed another round")
	}
	after, _ := json.Marshal(rs)
	if string(before) != string(after) {
		t.Fatal("failed resubmit changed state")
	}

	one := startedReview(t, 1)
	applyReview(t, one, findingHeader+findingOne, "author_reply", "")
	applyReview(t, one, authorHeader+"| r1-1 | ACCEPT | 수정 필요 |\n", "", "limit")
}

func TestReviewInputAndTransitions(t *testing.T) {
	for _, mutate := range []func(*ReviewInput){
		func(in *ReviewInput) { in.Diff = nil },
		func(in *ReviewInput) { in.Diff.HeadSHA = "wrong" },
		func(in *ReviewInput) { in.Ref.Round = 2 },
		func(in *ReviewInput) { in.Ref.PartID = "" },
		func(in *ReviewInput) { in.Ref.HeadSHA = in.Ref.BaseSHA },
	} {
		rs, in := NewReviewState(3), protocolInput("head-one")
		mutate(&in)
		if _, err := rs.Start(in); err == nil || !reflect.DeepEqual(rs, NewReviewState(3)) {
			t.Fatalf("invalid input changed state: %+v", rs)
		}
	}
	if _, err := NewReviewState(0).Start(protocolInput("head-one")); err == nil {
		t.Fatal("invalid limit accepted")
	}
	rs := startedReview(t, 3)
	if _, err := rs.Start(protocolInput("head-one")); err == nil {
		t.Fatal("second start accepted")
	}
	if _, err := rs.Resubmit(protocolInput("head-two")); err == nil {
		t.Fatal("premature resubmit accepted")
	}
	applyReview(t, rs, findingHeader+findingOne, "author_reply", "")
	applyReview(t, rs, authorHeader+"| r1-1 | ACCEPT | 수정 필요 |\n", "", "fix")
	for _, in := range []ReviewInput{protocolInput("head-one"), func() ReviewInput { in := protocolInput("head-two"); in.Ref.PartID = "p2"; return in }(), func() ReviewInput { in := protocolInput("head-two"); in.Ref.BaseSHA = "other-base"; return in }()} {
		before, _ := json.Marshal(rs)
		if _, err := rs.Resubmit(in); err == nil {
			t.Fatal("invalid resubmit accepted")
		}
		after, _ := json.Marshal(rs)
		if string(before) != string(after) {
			t.Fatal("resubmit error changed state")
		}
	}
}

func TestReviewRecheckLateErrorAndCarriedItems(t *testing.T) {
	rs := startedReview(t, 3)
	applyReview(t, rs, findingHeader+findingOne, "author_reply", "")
	applyReview(t, rs, authorHeader+"| r1-1 | ACCEPT | 수정 필요 |\n", "", "fix")
	if _, err := rs.Resubmit(protocolInput("head-two")); err != nil {
		t.Fatal(err)
	}
	// The recheck is valid, but a later semantic error must undo its FIXED.
	assertReviewErrorUnchanged(t, rs, recheckHeader+"| r1-1 | FIXED | 수정 확인 |\n"+findingHeader+findingTwo)
	applyReview(t, rs, recheckHeader+"| r1-1 | UNFIXED | 여전히 재현됨 |\n"+findingHeader, "", "fix")
	if _, err := rs.Resubmit(protocolInput("head-three")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rs.Pending.Prompt, "r1-1") || !strings.Contains(rs.Pending.Prompt, "UNFIXED") {
		t.Fatal("recheck dropped an unresolved item from an older round")
	}
	applyReview(t, rs, recheckHeader+"| r1-1 | FIXED | 수정 확인 |\n"+findingHeader, "", "passed")
	if len(rs.Rounds) != 3 || rs.Summary != (ReviewSummary{Complete: true, Passed: true}) {
		t.Fatalf("last allowed round must be able to pass: %+v", rs)
	}
}

func TestReviewPromptsAndInputOwnership(t *testing.T) {
	in := protocolInput("head-one")
	in.Diff.Files[0].Patch = strings.Repeat("가", reviewPatchRunes+10)
	in.Diff.Files = append(in.Diff.Files, DiffFile{Path: "src/last.go", Status: "A", Patch: "+last"})
	rs := NewReviewState(3)
	step, err := rs.Start(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{in.Goal, in.Brief, "src/", "base-commit", "head-one", "src/last.go", "diff 잘림: true", "worktree에서 파일을 직접 읽을 수", findingHeader, "대화 이력은 제공하지 않는다", "당신의 역할:", "강점은 우선순위일 뿐 다른 관점의 지적을 막지 않는다", "표만 쓰고 코드펜스로 감싸지 않는다"} {
		if !strings.Contains(step.Prompt, text) {
			t.Errorf("missing prompt text %q", text)
		}
	}
	if !utf8.ValidString(step.Prompt) || strings.Contains(step.Prompt, strings.Repeat("가", reviewPatchRunes+1)) {
		t.Fatal("patch clipping is invalid")
	}
	in.Scope[0] = "changed"
	in.Diff.Files[0].Path = "changed"
	step.Kind = "changed"
	applyReview(t, rs, findingHeader+findingOne, "author_reply", "")
	if strings.Contains(rs.Pending.Prompt, "changed") || !strings.Contains(rs.Pending.Prompt, "당신의 역할:") {
		t.Fatal("caller mutated retained input or pending step")
	}
	applyReview(t, rs, authorHeader+"| r1-1 | REJECT | 호출자 검사 확인 |\n", "rebuttal", "")
	for _, text := range []string{rebuttalHeader, "MAINTAIN", "WITHDRAW", "r1-1", "호출자 검사 확인", in.Goal, "head-one"} {
		if !strings.Contains(rs.Pending.Prompt, text) {
			t.Errorf("missing rebuttal context %q", text)
		}
	}
	data, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	var restored ReviewState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	applyReview(t, &restored, rebuttalHeader+"| r1-1 | WITHDRAW | 확인 |\n", "", "passed")
}

func TestReviewTablesWithoutLabelsAndWithProse(t *testing.T) {
	for name, reply := range map[string]string{
		"bare table":                 findingHeader,
		"observed CLI reply":         "참조 SHA와 두 파일을 읽어서 확인할게. 코드는 수정하거나 테스트를 실행하지 않을게.\n\n\n" + findingHeader,
		"prose before and after":     "파일을 확인했어.\n" + findingHeader + "추가 지적은 없어.",
		"code fence with pipes":      "예시 코드:\n```go\nif a || b {\n\t// | 이름 | 값 |\n\t// | --- | --- |\n}\n```\n" + findingHeader + "\n```markdown\n| 번호 | 심각도 | 파일:줄 | 재현 시나리오 | 확신도 |\n| --- | --- | --- | --- | --- |\n| 1 | critical | head:src/fake.go:1 | 코드펜스 안 가짜 표 | high |\n```",
		"tilde fence with fake rows": "~~~text\n| 1 | high | head:src/fake.go:1 | 무시 | high |\n~~~\n" + findingHeader,
		"prose with pipes":           "조건 a | b 분기와 `|` 구분자를 확인했어.\n| 시작만 파이프인 설명\n끝만 파이프인 설명 |\n| 첫 번째 인용 줄 |\n| 두 번째 인용 줄 |\n\n" + findingHeader + "| 참고: 표 바로 다음 단일 파이프 문장\n검토 완료: x | y 케이스도 정상.",
	} {
		t.Run(name, func(t *testing.T) {
			applyReview(t, startedReview(t, 3), reply, "", "passed")
		})
	}
	rs := startedReview(t, 3)
	applyReview(t, rs, "지적할 항목이 있어.\n"+findingHeader+findingOne+"코드 확인을 마쳤어.", "author_reply", "")
	applyReview(t, rs, "호출 경로를 확인했어.\n"+authorHeader+"| r1-1 | REJECT | 기존 검증 있음 |\n"+"근거를 표에 적었어.", "rebuttal", "")
	applyReview(t, rs, "반박을 확인했어.\n"+rebuttalHeader+"| r1-1 | WITHDRAW | 기존 검증 확인 |\n"+"지적을 철회해.", "", "passed")
}

func TestReviewRecheckTableOrderAndPresence(t *testing.T) {
	recheck := recheckHeader + "| r1-1 | FIXED | 수정 확인 |\n"
	for _, reverse := range []bool{false, true} {
		for _, separator := range []string{"", "\n", "\n수정과 새 지적을 각각 확인했어.\n"} {
			rs := startedReview(t, 3)
			applyReview(t, rs, findingHeader+findingOne, "author_reply", "")
			applyReview(t, rs, authorHeader+"| r1-1 | ACCEPT | 수정 필요 |\n", "", "fix")
			if _, err := rs.Resubmit(protocolInput("head-two")); err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []string{recheck, findingHeader, recheck + findingHeader + findingHeader, recheck + recheck + findingHeader} {
				assertReviewErrorUnchanged(t, rs, invalid)
			}
			first, second := recheck, findingHeader+findingOne
			if reverse {
				first, second = second, first
			}
			applyReview(t, rs, "검토 결과야.\n"+first+separator+second+"검토를 마쳤어.", "author_reply", "")
			if rs.Rounds[0].Items[0].Status != "fixed" || rs.Rounds[1].Items[0].Finding.ID != "r2-1" {
				t.Fatal("table order changed the meaning of recheck and findings")
			}
		}
	}
}

func roundTripReview(t *testing.T, rs *ReviewState) *ReviewState {
	t.Helper()
	data, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	var restored ReviewState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rs, &restored) {
		t.Fatalf("JSON round trip lost review state in phase %q", rs.Phase)
	}
	if restored.Pending != nil && restored.Pending.Prompt == "" {
		t.Fatalf("restored %s turn cannot be executed without its prompt", restored.Phase)
	}
	return &restored
}

func TestReviewResumeAfterJSONRoundTrip(t *testing.T) {
	rs := roundTripReview(t, startedReview(t, 3))
	assertReviewErrorUnchanged(t, rs, findingHeader+findingTwo)
	applyReview(t, rs, findingHeader+findingOne+findingTwo, "author_reply", "")

	rs = roundTripReview(t, rs)
	applyReview(t, rs, authorHeader+"| r1-1 | ACCEPT | 검증 수정 |\n| r1-2 | REJECT | 기존 검증 있음 |\n", "rebuttal", "")

	rs = roundTripReview(t, rs)
	applyReview(t, rs, rebuttalHeader+"| r1-2 | WITHDRAW | 기존 검증 확인 |\n", "", "fix")

	rs = roundTripReview(t, rs)
	if _, err := rs.Resubmit(protocolInput("head-two")); err != nil {
		t.Fatal(err)
	}
	rs = roundTripReview(t, rs)
	applyReview(t, rs, recheckHeader+"| r1-1 | FIXED | 수정 확인 |\n"+findingHeader+findingOne, "author_reply", "")
	if !strings.Contains(rs.Pending.Prompt, "head-two") || rs.Rounds[1].Items[0].Finding.ID != "r2-1" {
		t.Fatal("restored recheck did not build the next step from the current input")
	}

	rs = roundTripReview(t, rs)
	applyReview(t, rs, authorHeader+"| r2-1 | ACCEPT | 새 항목 수정 |\n", "", "fix")
	rs = roundTripReview(t, rs)
	if _, err := rs.Resubmit(protocolInput("head-three")); err != nil {
		t.Fatal(err)
	}
	rs = roundTripReview(t, rs)
	applyReview(t, rs, recheckHeader+"| r2-1 | FIXED | 수정 확인 |\n"+findingHeader, "", "passed")
	rs = roundTripReview(t, rs)
	if rs.Summary != (ReviewSummary{Complete: true, Passed: true}) || len(rs.Rounds) != 3 {
		t.Fatalf("resumed review did not pass: %+v", rs)
	}
}
