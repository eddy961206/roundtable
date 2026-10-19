package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestImageProviderArgumentsAndClaudeJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample image.png")
	data := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"", "existing-session"} {
		for _, mode := range []string{ModeChat, ModeRead, ModeWrite} {
			o := RunOptions{Prompt: "image prompt", SessionID: session, Mode: mode, Workspace: dir, ScratchDir: dir, Images: []Image{{Path: path, Mime: "image/png"}, {Path: path, Mime: "image/png"}}}
			args := codexArgs(o, "")
			count := 0
			for i, arg := range args {
				if arg == "--image" {
					count++
					if args[i+1] != path {
						t.Fatal(args)
					}
				}
			}
			if count != 2 {
				t.Fatal(args)
			}
			if session != "" && (args[1] != "resume" || args[len(args)-2] != session || slices.Index(args, "--image") > slices.Index(args, session)) {
				t.Fatal(args)
			}
			for _, args := range [][]string{agyArgs(o), claudeArgs(o)} {
				found := false
				for i := 0; i+1 < len(args); i++ {
					if args[i] == "--add-dir" && args[i+1] == dir {
						found = true
					}
				}
				if !found {
					t.Fatalf("attachments folder missing: %v", args)
				}
			}
			line, err := claudeImageLine(o)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Type    string `json:"type"`
				Message struct {
					Role    string `json:"role"`
					Content []struct {
						Type   string `json:"type"`
						Text   string `json:"text"`
						Source struct {
							Type      string `json:"type"`
							MediaType string `json:"media_type"`
							Data      string `json:"data"`
						} `json:"source"`
					} `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &got); err != nil {
				t.Fatal(err)
			}
			if got.Type != "user" || got.Message.Role != "user" || len(got.Message.Content) != 3 || got.Message.Content[0].Text != o.Prompt {
				t.Fatalf("%s", line)
			}
			for _, block := range got.Message.Content[1:] {
				decoded, err := base64.StdEncoding.DecodeString(block.Source.Data)
				if err != nil || !bytes.Equal(decoded, data) || block.Type != "image" || block.Source.Type != "base64" || block.Source.MediaType != "image/png" {
					t.Fatalf("%s", line)
				}
			}
			if line[len(line)-1] != '\n' {
				t.Fatal("stream-json needs a newline")
			}
		}
	}
}

func TestImageDeliveryReadFailure(t *testing.T) {
	o := RunOptions{Images: []Image{{Path: filepath.Join(t.TempDir(), "missing.png"), Mime: "image/png"}}}
	if _, err := claudeImageLine(o); err == nil {
		t.Fatal("missing image was silently omitted")
	}
	if err := validateImages(o.Images); err == nil {
		t.Fatal("missing image accepted")
	}
	plain, err := claudeImageLine(RunOptions{Prompt: "text only"})
	if err != nil || !bytes.Equal(plain, claudeUserLine("text only")) {
		t.Fatalf("text shape changed: %s %v", plain, err)
	}
}
