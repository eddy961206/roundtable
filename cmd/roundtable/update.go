package main

import (
	"fmt"
	"os"
	"time"

	"roundtable/internal/update"
)

// envOr lets a service set an update-check flag's default without editing its arguments.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// updateConfig turns the -update-* flags into a validated checker config.
func updateConfig(check, repo, branch, interval, api string) (update.Config, error) {
	cfg := update.Config{Repo: repo, Branch: branch, API: api, Build: update.ReadBuild()}
	switch check {
	case "off":
		return cfg, nil
	case "on":
		cfg.Enabled = true
	default:
		return cfg, fmt.Errorf("-update-check: on 또는 off만 허용: %q", check)
	}
	d, err := time.ParseDuration(interval)
	if err != nil {
		return cfg, fmt.Errorf("-update-interval: %v", err)
	}
	cfg.Interval = d
	return cfg, cfg.Validate()
}
