//go:build windows

package server

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"roundtable/internal/agent"
)

const canOpenNativeSession = true

func powershellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func nativeCommand(plan agent.NativeSession) string {
	args := make([]string, len(plan.Args))
	for i, arg := range plan.Args {
		args[i] = syscall.EscapeArg(arg)
	}
	// Start-Process receives one Windows command line so Windows PowerShell 5
	// preserves empty arguments such as Claude's --tools "".
	command := "Set-Location -LiteralPath " + powershellLiteral(plan.Directory) + "\r\n" +
		"Start-Process -FilePath " + powershellLiteral(plan.Executable) +
		" -ArgumentList " + powershellLiteral(strings.Join(args, " ")) +
		" -WorkingDirectory " + powershellLiteral(plan.Directory) + " -NoNewWindow -Wait"
	if len(plan.Environment) == 0 {
		return command
	}
	keys := make([]string, 0, len(plan.Environment))
	for key := range plan.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var script strings.Builder
	script.WriteString("& {\r\n$roundtableSavedEnv = @{}\r\n")
	for _, key := range keys {
		quoted := powershellLiteral(key)
		script.WriteString("$roundtableSavedEnv[" + quoted + "] = [Environment]::GetEnvironmentVariable(" + quoted + ", 'Process')\r\n")
	}
	script.WriteString("try {\r\n")
	for _, key := range keys {
		script.WriteString("[Environment]::SetEnvironmentVariable(" + powershellLiteral(key) + ", " + powershellLiteral(plan.Environment[key]) + ", 'Process')\r\n")
	}
	script.WriteString(command + "\r\n} finally {\r\n")
	for _, key := range keys {
		quoted := powershellLiteral(key)
		script.WriteString("[Environment]::SetEnvironmentVariable(" + quoted + ", $roundtableSavedEnv[" + quoted + "], 'Process')\r\n")
	}
	script.WriteString("}\r\n}")
	return script.String()
}

func openNativeSession(plan agent.NativeSession, onExit func()) (int, error) {
	script := "$ErrorActionPreference = 'Stop'\r\n" +
		"$env:CLAUDECODE = $null\r\n$env:CLAUDE_CODE_ENTRYPOINT = $null\r\n$env:CLAUDE_CODE_SSE_PORT = $null\r\n" + nativeCommand(plan)
	units := utf16.Encode([]rune(script))
	encoded := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(encoded[i*2:], unit)
	}
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	application, err := syscall.UTF16PtrFromString(powershell)
	if err != nil {
		return 0, err
	}
	command, err := syscall.UTF16PtrFromString(syscall.EscapeArg(powershell) + " -NoLogo -NoProfile -NoExit -EncodedCommand " + base64.StdEncoding.EncodeToString(encoded))
	if err != nil {
		return 0, err
	}
	directory, err := syscall.UTF16PtrFromString(plan.Directory)
	if err != nil {
		return 0, err
	}
	// os/exec connects nil streams to NUL and sets STARTF_USESTDHANDLES.
	// A native TUI instead needs the input/output handles of its new console.
	startup := syscall.StartupInfo{}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	var process syscall.ProcessInformation
	if err := syscall.CreateProcess(application, command, nil, nil, false, 0x00000010, nil, directory, &startup, &process); err != nil {
		return 0, err
	}
	_ = syscall.CloseHandle(process.Thread)
	go func() {
		_, _ = syscall.WaitForSingleObject(process.Process, syscall.INFINITE)
		_ = syscall.CloseHandle(process.Process)
		onExit()
	}()
	return int(process.ProcessId), nil
}
