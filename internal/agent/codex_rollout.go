package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// rolloutWatcher finds Codex sub-agent threads: every spawned agent gets its
// own rollout-*.jsonl whose session_meta carries parent_thread_id.
type rolloutWatcher struct {
	mu      sync.Mutex
	root    string
	since   time.Time
	known   map[string]*rolloutFile // path -> state
	threads map[string]bool         // thread ids that belong to this turn (root + children)
}

type rolloutFile struct {
	path    string
	id      string
	parent  string
	name    string
	offset  int64
	started bool
	ended   bool
	model   string // last turn_context model/effort already emitted
	effort  string
}

func newRolloutWatcher(root string, since time.Time) *rolloutWatcher {
	return &rolloutWatcher{root: root, since: since, known: map[string]*rolloutFile{}, threads: map[string]bool{}}
}

// scan looks for new child rollouts of thread and streams their new lines.
func (w *rolloutWatcher) scan(thread string, emit func(Event)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.threads[thread] = true
	for _, path := range w.candidates() {
		rf, ok := w.known[path]
		if !ok {
			meta := readSessionMeta(path)
			if meta == nil {
				continue
			}
			rf = &rolloutFile{path: path, id: meta.id, parent: meta.parent, name: meta.name}
			w.known[path] = rf
		}
		if rf.id == thread {
			w.tailAsks(rf, emit)
			continue
		}
		if rf.id == "" || !w.threads[rf.parent] {
			continue
		}
		w.threads[rf.id] = true
		if !rf.started {
			rf.started = true
			x := ev(KindSubagentStart)
			x.Parent = rf.id
			x.Name = rf.name
			x.Meta = map[string]any{"parent_thread": rf.parent, "file": filepath.Base(path)}
			emit(x)
		}
		w.tail(rf, emit)
	}
}

// pathOf returns the rollout file of a thread id seen during scanning.
func (w *rolloutWatcher) pathOf(thread string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, rf := range w.known {
		if rf.id == thread {
			return path
		}
	}
	for _, path := range w.candidates() {
		if strings.Contains(filepath.Base(path), thread) {
			return path
		}
	}
	return ""
}

// finish emits subagent_end for children that never wrote task_complete.
func (w *rolloutWatcher) finish(emit func(Event)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, rf := range w.known {
		if rf.started && !rf.ended {
			rf.ended = true
			x := ev(KindSubagentEnd)
			x.Parent = rf.id
			emit(x)
		}
	}
}

func (w *rolloutWatcher) candidates() []string {
	var out []string
	seenDays := map[string]bool{}
	for _, day := range []time.Time{w.since, time.Now()} {
		dir := filepath.Join(w.root, day.Format("2006"), day.Format("01"), day.Format("02"))
		if seenDays[dir] {
			continue
		}
		seenDays[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "rollout-") || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().Before(w.since) {
				continue
			}
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

type sessionMeta struct{ id, parent, name string }

func readSessionMeta(path string) *sessionMeta {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return nil
	}
	m := parseLine(line)
	if m == nil || str(m, "type") != "session_meta" {
		return nil
	}
	p := obj(m, "payload")
	meta := &sessionMeta{id: str(p, "id"), parent: str(p, "parent_thread_id")}
	nick := str(p, "agent_nickname")
	path2 := str(p, "agent_path")
	meta.name = strings.TrimSpace(nick + " " + path2)
	if meta.name == "" {
		meta.name = "codex subagent"
	}
	return meta
}

func (w *rolloutWatcher) tail(rf *rolloutFile, emit func(Event)) {
	f, err := os.Open(rf.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(rf.offset, 0); err != nil {
		return
	}
	r := takeTranscriptReader(f)
	defer putTranscriptReader(r)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			break // incomplete trailing line stays for the next scan
		}
		rf.offset += int64(len(line))
		for _, x := range rolloutEvents(rf.id, line) {
			switch x.Kind {
			case KindSubagentEnd:
				rf.ended = true
			case KindSubagentMeta:
				// Every turn repeats turn_context; only a change is news.
				model := x.Meta["model"].(string)
				effort, _ := x.Meta["effort"].(string)
				if model == rf.model && effort == rf.effort {
					continue
				}
				rf.model, rf.effort = model, effort
			}
			emit(x)
		}
	}
}

// tailAsks reads the turn's own rollout for questions to the person. codex exec
// --json prints request_user_input_async only as a plain agent_message (title and
// "- option" lines); the rollout keeps the call id and the structured questions.
// A resumed thread's rollout holds earlier turns, so lines before since are skipped.
func (w *rolloutWatcher) tailAsks(rf *rolloutFile, emit func(Event)) {
	f, err := os.Open(rf.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(rf.offset, 0); err != nil {
		return
	}
	r := takeTranscriptReader(f)
	defer putTranscriptReader(r)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			break // incomplete trailing line stays for the next scan
		}
		rf.offset += int64(len(line))
		if x, ok := rolloutAsk(line, w.since); ok {
			emit(x)
		}
	}
}

// rolloutAsk converts a request_user_input_async call written at or after since.
func rolloutAsk(line string, since time.Time) (Event, bool) {
	if !strings.Contains(line, `"request_user_input_async"`) {
		return Event{}, false
	}
	m := parseLine(line)
	p := obj(m, "payload")
	if str(m, "type") != "response_item" || str(p, "type") != "function_call" || str(p, "name") != "request_user_input_async" {
		return Event{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, str(m, "timestamp"))
	if err != nil || at.Before(since) {
		return Event{}, false
	}
	var args struct {
		Questions []AskQuestion `json:"questions"`
	}
	err = json.Unmarshal([]byte(str(p, "arguments")), &args)
	if err == nil && len(args.Questions) == 0 {
		err = fmt.Errorf("questions가 비어 있음")
	}
	if err != nil {
		// The CLI accepted the call, so the person was asked; say so in the trace.
		x := ev(KindStatus)
		x.Text = fmt.Sprintf("사용자 질문(%s)의 인자를 해석하지 못함: %v", str(p, "call_id"), err)
		return x, true
	}
	x := ev(KindAsk)
	x.ID = str(p, "call_id")
	x.Meta = map[string]any{"questions": args.Questions}
	return x, true
}

// rolloutEvents converts one line of a Codex rollout file into subagent events.
func rolloutEvents(threadID, line string) []Event {
	m := parseLine(line)
	if m == nil {
		return nil
	}
	p := obj(m, "payload")
	switch str(m, "type") {
	case "event_msg":
		if str(p, "type") == "task_complete" {
			x := ev(KindSubagentEnd)
			x.Parent = threadID
			x.Output = str(p, "last_agent_message")
			return []Event{x}
		}
		return nil
	case "turn_context":
		// subagent_meta always names the model; effort only when reported.
		model := str(p, "model")
		if model == "" {
			return nil
		}
		x := ev(KindSubagentMeta)
		x.Parent = threadID
		x.Meta = map[string]any{"model": model}
		if effort := str(p, "effort"); effort != "" {
			x.Meta["effort"] = effort
		}
		return []Event{x}
	case "response_item":
	default:
		return nil
	}
	x := ev(KindSubagentMsg)
	x.Parent = threadID
	switch str(p, "type") {
	case "message":
		role := str(p, "role")
		if role == "developer" {
			return nil
		}
		text := contentText(p["content"])
		if role == "user" && (strings.HasPrefix(text, "# AGENTS.md") || strings.HasPrefix(text, "<environment_context>") || strings.HasPrefix(text, "<INSTRUCTIONS>")) {
			return nil
		}
		x.Role, x.Text = role, text
	case "agent_message":
		x.Role = "message"
		x.Name = str(p, "author") + " → " + str(p, "recipient")
		x.Text = contentText(p["content"])
	case "custom_tool_call", "function_call":
		x.Role, x.ID, x.Name = "tool_call", str(p, "call_id"), str(p, "name")
		x.Input = firstNonEmpty(str(p, "input"), str(p, "arguments"))
	case "custom_tool_call_output", "function_call_output":
		x.Role, x.ID = "tool_result", str(p, "call_id")
		x.Output = contentText(p["output"])
	default:
		return nil
	}
	return []Event{x}
}
