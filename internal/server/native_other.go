//go:build !windows

package server

import (
	"fmt"
	"sort"
	"strings"

	"roundtable/internal/agent"
)

const canOpenNativeSession = false

func shellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func nativeCommand(plan agent.NativeSession) string {
	var args []string
	if len(plan.Environment) > 0 {
		args = append(args, "env")
		keys := make([]string, 0, len(plan.Environment))
		for key := range plan.Environment {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			args = append(args, shellLiteral(key+"="+plan.Environment[key]))
		}
	}
	args = append(args, shellLiteral(plan.Executable))
	for _, arg := range plan.Args {
		args = append(args, shellLiteral(arg))
	}
	return "cd " + shellLiteral(plan.Directory) + " && " + strings.Join(args, " ")
}

func openNativeSession(plan agent.NativeSession, onExit func()) (int, error) {
	return 0, fmt.Errorf("원본 CLI에서 이어보기 명령을 실행하세요")
}
