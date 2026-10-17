package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const (
	maxLineBytes   = 16 * 1024 * 1024
	maxStderrBytes = 64 * 1024
)

// resolveCommand walks PATH in order and returns a real executable for name.
// On Windows an npm shim (codex.cmd) is swapped for the binary it wraps so the
// process can be spawned and killed without cmd.exe, and so the Codex build
// that ships its code-mode host is the one that runs.
func resolveCommand(name string) (string, error) {
	if filepath.IsAbs(name) {
		if !fileExists(name) {
			return "", fmt.Errorf("%s: 실행 파일이 없거나 디렉터리임", name)
		}
		return name, nil
	}
	if runtime.GOOS != "windows" {
		return exec.LookPath(name)
	}
	hasExt := filepath.Ext(name) != ""
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if hasExt {
			if p := filepath.Join(dir, name); fileExists(p) {
				return p, nil
			}
			continue
		}
		if p := filepath.Join(dir, name+".exe"); fileExists(p) {
			return p, nil
		}
		for _, ext := range []string{".cmd", ".bat"} {
			if fileExists(filepath.Join(dir, name+ext)) {
				if v := npmVendoredExe(dir, name); v != "" {
					return v, nil
				}
				break
			}
		}
	}
	return "", fmt.Errorf("%s: 실행 파일을 PATH에서 찾지 못함 (npm 셸 스크립트만 있으면 -%s 옵션으로 exe 경로를 지정)", name, name)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// npmVendoredExe finds the native binary behind an npm shim such as
// %APPDATA%/npm/codex.cmd (package @openai/codex).
func npmVendoredExe(dir, name string) string {
	patterns := []string{
		filepath.Join(dir, "node_modules", "@opencode", "cli", "bin", name+".exe"),
		filepath.Join(dir, "node_modules", "@openai", name, "node_modules", "@openai", name+"-win32-x64", "vendor", "*", "bin", name+".exe"),
		filepath.Join(dir, "node_modules", "@openai", name, "vendor", "*", "codex", name+".exe"),
		filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "vendor", "*", name+".exe"),
	}
	for _, pat := range patterns {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// runLines starts cmd, feeds stdin, and calls onLine for every stdout line.
// It kills the whole process tree when ctx is cancelled.
// When cmd.Stdin is already set (e.g. a StdinPipe for live steering) it is left alone.
func runLines(ctx context.Context, cmd *exec.Cmd, stdin string, onLine func(string)) (stderr string, err error) {
	if cmd.Stdin == nil {
		cmd.Stdin = strings.NewReader(stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return "", err
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = errPipe.Close()
		return "", err
	}

	var wg sync.WaitGroup
	var errBuf strings.Builder
	var readErrors [2]error
	var terminateOnce sync.Once
	terminate := func() {
		terminateOnce.Do(func() {
			// A descendant may keep inherited output handles open after the
			// CLI exits. Cancellation must unblock our readers even in that case.
			_ = stdout.Close()
			_ = errPipe.Close()
			killTree(cmd)
		})
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer errPipe.Close()
		sc := bufio.NewScanner(errPipe)
		sc.Buffer(make([]byte, 64*1024), maxLineBytes)
		for sc.Scan() {
			if remaining := maxStderrBytes - errBuf.Len(); remaining > 0 {
				line := sc.Bytes()
				if len(line) > remaining {
					line = line[:remaining]
				}
				errBuf.Write(line)
				if errBuf.Len() < maxStderrBytes {
					errBuf.WriteByte('\n')
				}
			}
		}
		if err := sc.Err(); err != nil {
			readErrors[0] = fmt.Errorf("CLI stderr 읽기 실패: %w", err)
			terminate()
		}
	}()
	go func() {
		defer wg.Done()
		defer stdout.Close()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), maxLineBytes)
		for sc.Scan() {
			onLine(sc.Text())
		}
		if err := sc.Err(); err != nil {
			readErrors[1] = fmt.Errorf("CLI stdout 읽기 실패: %w", err)
			terminate()
		}
	}()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			terminate()
		case <-done:
		}
	}()
	wg.Wait()
	waitErr := cmd.Wait()
	close(done)
	if ctx.Err() != nil {
		return errBuf.String(), ctx.Err()
	}
	if err := errors.Join(readErrors[:]...); err != nil {
		return errBuf.String(), err
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return errBuf.String(), fmt.Errorf("exit %d", exitErr.ExitCode())
		}
		return errBuf.String(), waitErr
	}
	return errBuf.String(), nil
}

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		k := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
		hideWindow(k)
		_ = k.Run()
		return
	}
	if !killGroup(cmd) {
		_ = cmd.Process.Kill()
	}
}

// envWithout returns the current environment minus the given keys.
func envWithout(keys ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		skip := false
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return out
}
