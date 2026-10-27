package main

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const (
	wsaeacces     = syscall.Errno(10013)
	wsaeaddrinuse = syscall.Errno(10048)
)

func addrInUse(err error) bool { return errors.Is(err, wsaeaddrinuse) }

// explainListenError names the Windows excluded port range (Hyper-V, WinNAT,
// Docker reserve these) that makes bind fail with an access error.
func explainListenError(addr string, err error) error {
	if !errors.Is(err, wsaeacces) {
		return err
	}
	_, portText, splitErr := net.SplitHostPort(addr)
	port, atoiErr := strconv.Atoi(portText)
	if splitErr != nil || atoiErr != nil {
		return err
	}
	out, cmdErr := exec.Command("netsh", "interface", "ipv4", "show", "excludedportrange", "protocol=tcp").Output()
	if cmdErr != nil {
		return fmt.Errorf("%w (Windows 예약 포트 범위일 수 있음. netsh로 확인하지 못함: %v)", err, cmdErr)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		start, errStart := strconv.Atoi(fields[0])
		end, errEnd := strconv.Atoi(fields[1])
		if errStart == nil && errEnd == nil && start <= port && port <= end {
			return fmt.Errorf("%w: %d번 포트는 Windows가 예약한 TCP 범위 %d~%d 안에 있음(Hyper-V·WinNAT 등). -addr 127.0.0.1:18787처럼 범위 밖 포트를 지정해. 범위 확인: netsh interface ipv4 show excludedportrange protocol=tcp", err, port, start, end)
		}
	}
	return fmt.Errorf("%w (Windows 예약 포트 범위에는 없음. 다른 프로그램이 독점 사용 중인지 확인해)", err)
}
