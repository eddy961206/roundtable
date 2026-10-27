package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"roundtable/internal/server"
)

// listen retries an address in use until wait passes; any other error, or a
// zero wait, fails at once with the cause explained.
func listen(addr string, wait time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(wait)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		if !addrInUse(err) || time.Now().After(deadline) {
			return nil, explainListenError(addr, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// runRestart stops the server at --url once no turn runs or waits, then serves
// in this process with the stopped server's arguments and working directory.
// Run the new binary's restart; the server then runs that binary.
func runRestart(args []string) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	base := fs.String("url", "http://127.0.0.1:8787", "running Roundtable HTTP URL (loopback only)")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long to wait for running and queued turns before giving up")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *timeout < time.Second {
		fmt.Fprintln(os.Stderr, "사용법: roundtable restart --url http://127.0.0.1:8787 [--timeout 30m]")
		return 2
	}
	u, err := url.Parse(*base)
	if err != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		fmt.Fprintln(os.Stderr, "--url은 http://127.0.0.1:<포트> 같은 loopback 주소여야 함")
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "%s: 실행·대기 중인 턴이 끝나기를 최대 %s 기다림\n", *base, *timeout)
	stopped, err := stopWhenIdle(*base, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "재기동하지 않음:", err)
		return 1
	}
	if stopped.Error != "" {
		fmt.Fprintln(os.Stderr, "이전 서버가 종료 기록을 저장하지 못함:", stopped.Error)
	}
	if err := os.Chdir(stopped.Dir); err != nil {
		fmt.Fprintln(os.Stderr, "이전 서버의 작업 폴더로 이동하지 못함:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "이전 서버 pid %d(%s)가 유휴 상태에서 종료함. 같은 인자로 %s를 시작함: %q\n", stopped.PID, stopped.Exe, self, stopped.Args[1:])
	os.Args = append([]string{self}, stopped.Args[1:]...)
	serve(30 * time.Second)
	return 0
}

func stopWhenIdle(base string, timeout time.Duration) (*server.ShutdownResult, error) {
	body, err := json.Marshal(server.ShutdownRequest{WhenIdle: true, TimeoutSeconds: int(timeout / time.Second)})
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: timeout + time.Minute}
	resp, err := client.Post(base+"/api/shutdown", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST /api/shutdown: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	var result server.ShutdownResult
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, fmt.Errorf("종료 응답 해석 실패: %w", err)
	}
	if len(result.Args) == 0 || result.Dir == "" {
		return nil, fmt.Errorf("종료 응답에 인자나 작업 폴더가 없음: %s", b)
	}
	return &result, nil
}

// runInstall copies this executable over each destination. Windows refuses to
// overwrite a running executable but allows renaming it, so an existing file is
// renamed to <dest>.old-<ms> first; processes started from it keep running.
func runInstall(args []string) int {
	const installUsage = "사용법: roundtable install <대상 exe 경로>...\n이 실행 파일을 각 대상에 복사한다. 대상이 실행 중이면 이름을 <대상>.old-<시각>으로 바꾼 뒤 복사한다."
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, installUsage)
		return 2
	}
	for _, a := range args {
		switch {
		case a == "-h" || a == "-help" || a == "--help" || a == "help":
			fmt.Println(installUsage)
			return 0
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "알 수 없는 옵션: %s\n%s\n", a, installUsage)
			return 2
		}
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, dest := range args {
		kept, err := install(self, dest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", dest, err)
			return 1
		}
		fmt.Printf("설치함: %s\n", dest)
		for _, old := range kept {
			fmt.Printf("  지우지 못한 이전 사본(실행 중인 프로세스가 쓰는 중일 수 있음): %s\n", old)
		}
	}
	return 0
}

// install returns the earlier copies it could not delete (still in use).
func install(self, dest string) ([]string, error) {
	dest, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	if a, errA := os.Stat(self); errA == nil {
		if b, errB := os.Stat(dest); errB == nil && os.SameFile(a, b) {
			return nil, fmt.Errorf("실행 중인 파일 자신에는 설치할 수 없음")
		}
	}
	src, err := os.Open(self)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	tmp := dest + ".new-" + stamp
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(out, src)
	if err := errors.Join(copyErr, out.Sync(), out.Close()); err != nil {
		return nil, errors.Join(err, os.Remove(tmp))
	}
	if _, err := os.Stat(dest); err == nil {
		if err := os.Rename(dest, dest+".old-"+stamp); err != nil {
			return nil, errors.Join(fmt.Errorf("기존 파일 이름 바꾸기: %w", err), os.Remove(tmp))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, dest); err != nil {
		return nil, err
	}
	olds, err := filepath.Glob(dest + ".old-*")
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, old := range olds {
		if err := os.Remove(old); err != nil {
			kept = append(kept, fmt.Sprintf("%s (%v)", old, err)) // typically a process started from that copy
		}
	}
	return kept, nil
}
