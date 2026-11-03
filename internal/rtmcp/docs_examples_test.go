package rtmcp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The guides and the orchestration scripts show `rt` commands that agents
// copy as they are. On 2026-11-02 a guide's `rt wait --timeout_seconds 7200`
// failed against the tool's limit (fixed in 9671ae1). This test reads every
// documented command and checks its name, its argument names and its literal
// numbers and choices against the tool schema.
var (
	docFiles = []string{"README.md", "docs/AGENT_GUIDE.md", "skills/roundtable/SKILL.md", "tools/orchestrate/README.md"}
	// A guide line: `roundtable rt [--url X] <command> ...`. The AGENT_GUIDE
	// index lists commands without the prefix in its table's first cell.
	prefixedRe = regexp.MustCompile("roundtable(?:\\.exe)? rt (?:--url \\S+ )?([a-z_]+)([^`\\n]*)")
	indexRe    = regexp.MustCompile("^\\| `([a-z_]+)((?: [^`]*)?)`")
	scriptRe   = regexp.MustCompile(`(?:^|[\s(|;&])rt ([a-z_]+)( .*)?$`)
	flagRe     = regexp.MustCompile(`--([a-z_]+)(?:[ =]("[^"]*"|'[^']*'|[^\s\]]+))?`)
)

// shellWaitMax is the shell form's own limit for rt wait (cli.go cliWaitChunks).
const shellWaitMax = 86400

type docCommand struct {
	where, name, args string
}

func documentedCommands(t *testing.T, root string) []docCommand {
	t.Helper()
	var out []docCommand
	for _, rel := range docFiles {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			where := rel + ":" + strconv.Itoa(i+1)
			for _, m := range prefixedRe.FindAllStringSubmatch(line, -1) {
				out = append(out, docCommand{where, m[1], m[2]})
			}
			if rel == "docs/AGENT_GUIDE.md" {
				if m := indexRe.FindStringSubmatch(line); m != nil {
					name, args := m[1], m[2]
					if name == "rt" { // a failure-table row names `rt post` with the prefix
						name, args, _ = strings.Cut(strings.TrimSpace(args)+" ", " ")
					}
					out = append(out, docCommand{where, name, args})
				}
			}
		}
	}
	scripts, err := filepath.Glob(filepath.Join(root, "tools", "orchestrate", "*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range scripts {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := scriptRe.FindStringSubmatch(line); m != nil {
				out = append(out, docCommand{filepath.Base(path) + ":" + strconv.Itoa(i+1), m[1], m[2]})
			}
		}
	}
	return out
}

// literal tells whether a documented value is a real value rather than a
// placeholder (<방ID>, N, $var, -) that the reader replaces.
func literal(v string) bool {
	return v != "" && !strings.ContainsAny(v, "<>$…") && v != "-" && v != "N"
}

func checkDocumented(s *Server, commands []docCommand) []string {
	tools := map[string]schema{}
	for _, tl := range s.tools() {
		tools[strings.TrimPrefix(tl.Name, "rt_")] = tl.InputSchema
	}
	var problems []string
	bad := func(c docCommand, format string, args ...any) {
		problems = append(problems, c.where+": "+fmt.Sprintf(format, args...))
	}
	for _, c := range commands {
		if c.name == "help" {
			continue
		}
		sc, ok := tools[c.name]
		if !ok {
			bad(c, "알 수 없는 명령 rt %s", c.name)
			continue
		}
		for _, m := range flagRe.FindAllStringSubmatch(c.args, -1) {
			key, value := m[1], strings.Trim(m[2], `"'`)
			if strings.HasPrefix(value, "--") {
				value = "" // `[--mode] --participants`: a flag shown without a value
			}
			if key == "help" || key == "url" {
				continue
			}
			prop, ok := sc.Properties[key]
			if !ok {
				bad(c, "rt %s에 없는 인자 --%s", c.name, key)
				continue
			}
			// "active\|dropped" in a table lists the choices.
			for _, v := range strings.Split(strings.ReplaceAll(value, `\|`, "|"), "|") {
				if !literal(v) {
					continue
				}
				switch prop.Type {
				case "integer":
					n, err := strconv.ParseInt(v, 10, 64)
					if err != nil {
						continue // a word in prose, not a number
					}
					low, high := prop.Minimum, prop.Maximum
					if c.name == "wait" && key == "timeout_seconds" {
						shell := int64(shellWaitMax)
						high = &shell
					}
					if (low != nil && n < *low) || (high != nil && n > *high) {
						bad(c, "rt %s --%s %d이 허용 범위 밖 (최소 %v, 최대 %v)", c.name, key, n, deref(low), deref(high))
					}
				case "boolean":
					if v != "true" && v != "false" {
						bad(c, "rt %s --%s %q는 true·false가 아님", c.name, key, v)
					}
				case "string":
					if len(prop.Enum) > 0 && !slices.Contains(prop.Enum, v) {
						bad(c, "rt %s --%s %q는 허용 값(%s)이 아님", c.name, key, v, strings.Join(prop.Enum, ", "))
					}
				}
			}
		}
	}
	return problems
}

func deref(p *int64) any {
	if p == nil {
		return "없음"
	}
	return *p
}

func TestDocumentedCommandsMatchTools(t *testing.T) {
	s, err := New("http://127.0.0.1:1", false)
	if err != nil {
		t.Fatal(err)
	}
	commands := documentedCommands(t, filepath.Join("..", ".."))
	if len(commands) < 30 {
		t.Fatalf("found only %d documented commands; did the guides move?", len(commands))
	}
	for _, p := range checkDocumented(s, commands) {
		t.Error(p)
	}
}

// The checker must catch the 2026-11-02 mistake and the other kinds of drift.
func TestDocumentedCommandCheckerCatchesDrift(t *testing.T) {
	s, err := New("http://127.0.0.1:1", false)
	if err != nil {
		t.Fatal(err)
	}
	var commands []docCommand
	for _, line := range []string{
		"`roundtable rt post --room R --text x --wait_seconds 7200`",
		"`roundtable rt wait --rooms A:1 --timeout_seconds 90000`",
		"`roundtable rt read --room R --after 3`",
		"`roundtable rt watch --room R`",
		"`roundtable rt update_decision --room R --id D --status proposed`",
		"`roundtable rt archive_rooms --rooms A --archived yes`",
	} {
		m := prefixedRe.FindStringSubmatch(line)
		commands = append(commands, docCommand{"case", m[1], m[2]})
	}
	if got := checkDocumented(s, commands); len(got) != 6 {
		t.Fatalf("want 6 problems, got %d: %v", len(got), got)
	}
	ok := []docCommand{{"ok", "wait", " --rooms <방>:<seq> --timeout_seconds 7200"}, {"ok", "update_decision", ` --room R --id D --status active\|dropped`}}
	if got := checkDocumented(s, ok); len(got) != 0 {
		t.Fatalf("valid commands were flagged: %v", got)
	}
}
