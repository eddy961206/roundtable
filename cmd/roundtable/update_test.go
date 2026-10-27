package main

import (
	"testing"
	"time"
)

func TestUpdateConfigFlags(t *testing.T) {
	const repo, api = "eddy961206/roundtable", "https://api.github.com"
	cfg, err := updateConfig("on", repo, "main", "6h", api)
	if err != nil || !cfg.Enabled || cfg.Interval != 6*time.Hour || cfg.Repo != repo {
		t.Fatalf("%+v %v", cfg, err)
	}
	// off needs no other valid setting and enables nothing.
	cfg, err = updateConfig("off", "", "", "", "")
	if err != nil || cfg.Enabled {
		t.Fatalf("%+v %v", cfg, err)
	}
	for _, bad := range [][5]string{
		{"yes", repo, "main", "6h", api},
		{"on", repo, "main", "soon", api},
		{"on", repo, "main", "1m", api},
		{"on", "not-a-repo", "main", "6h", api},
	} {
		if _, err := updateConfig(bad[0], bad[1], bad[2], bad[3], bad[4]); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestEnvOrSetsFlagDefault(t *testing.T) {
	t.Setenv("ROUNDTABLE_UPDATE_CHECK", "off")
	if envOr("ROUNDTABLE_UPDATE_CHECK", "on") != "off" || envOr("ROUNDTABLE_UNSET_FOR_TEST", "on") != "on" {
		t.Fatal("env default not applied")
	}
}
