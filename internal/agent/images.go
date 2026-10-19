package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func validateImages(images []Image) error {
	for _, im := range images {
		f, err := os.Open(im.Path)
		if err != nil {
			return fmt.Errorf("첨부 전달 실패: %w", err)
		}
		info, err := f.Stat()
		f.Close()
		if err != nil {
			return fmt.Errorf("첨부 전달 실패: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() > 10*1024*1024 {
			return fmt.Errorf("첨부 전달 실패: 잘못된 이미지 파일")
		}
	}
	return nil
}

func imageDirs(images []Image) []string {
	var dirs []string
	seen := map[string]bool{}
	for _, im := range images {
		dir := filepath.Dir(im.Path)
		if !seen[dir] {
			dirs = append(dirs, dir)
			seen[dir] = true
		}
	}
	return dirs
}

func claudeImageLine(o RunOptions) ([]byte, error) {
	if err := validateImages(o.Images); err != nil {
		return nil, err
	}
	content := []map[string]any{{"type": "text", "text": o.Prompt}}
	for _, im := range o.Images {
		b, err := os.ReadFile(im.Path)
		if err != nil {
			return nil, fmt.Errorf("첨부 전달 실패: %w", err)
		}
		content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": im.Mime, "data": base64.StdEncoding.EncodeToString(b)}})
	}
	b, err := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	return append(b, '\n'), err
}
