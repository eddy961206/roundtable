package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type CLIVersion struct {
	Version   string
	CheckedAt int64
	Error     string
}

// RefreshVersions coalesces concurrent refreshes and probes vendors in parallel.
// Version lookup never runs a model turn and never holds the room mutex.
func (r *Registry) RefreshVersions(ctx context.Context) error {
	r.versionMu.Lock()
	if flight := r.versionFlight; flight != nil {
		r.versionMu.Unlock()
		select {
		case <-flight:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	flight := make(chan struct{})
	r.versionFlight = flight
	r.versionMu.Unlock()
	defer func() {
		r.versionMu.Lock()
		r.versionFlight = nil
		close(flight)
		r.versionMu.Unlock()
	}()
	var wg sync.WaitGroup
	for _, p := range r.providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			version := probeVersion(ctx, p.Command())
			r.versionMu.Lock()
			if r.versions == nil {
				r.versions = map[string]CLIVersion{}
			}
			r.versions[p.ID()] = version
			r.versionMu.Unlock()
		}()
	}
	wg.Wait()
	return ctx.Err()
}

func probeVersion(parent context.Context, command string) CLIVersion {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	result := CLIVersion{}
	path, err := resolveCommand(command)
	if err == nil {
		cmd := exec.Command(path, "--version")
		var output strings.Builder
		tooLong := false
		stderr, runErr := runLines(ctx, cmd, "", func(line string) {
			if output.Len()+len(line)+1 > 4096 {
				tooLong = true
				cancel()
				return
			}
			output.WriteString(line + "\n")
		})
		err = runErr
		if tooLong {
			err = fmt.Errorf("버전 명령 응답이 4096바이트를 초과함")
		}
		if err == nil {
			result.Version = strings.TrimSpace(output.String())
			if result.Version == "" {
				err = fmt.Errorf("버전 명령이 빈 응답을 반환함")
			}
		} else if strings.TrimSpace(stderr) != "" {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
		}
	}
	result.CheckedAt = time.Now().UnixMilli()
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
