package room

import (
	"encoding/json"
	"fmt"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"roundtable/internal/agent"
)

const MaxAttachmentBytes int64 = 10 * 1024 * 1024
const MaxAttachments = 4

type Attachment struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Mime   string `json:"mime"`
	Bytes  int64  `json:"bytes"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	By     string `json:"by"`
	At     int64  `json:"at"`
}

var attachmentID = regexp.MustCompile(`^[a-f0-9]+$`)

func attachmentExt(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ""
}

func (s *Store) attachmentDir(roomID string) string {
	return filepath.Join(s.roomDir(roomID), "attachments")
}

func (s *Store) attachmentPath(roomID string, a Attachment) string {
	return filepath.Join(s.attachmentDir(roomID), a.ID+attachmentExt(a.Mime))
}

func (s *Store) saveAttachment(roomID, name, by string, source io.Reader) (Attachment, error) {
	var a Attachment
	b, err := io.ReadAll(io.LimitReader(source, MaxAttachmentBytes+1))
	if err != nil {
		return a, err
	}
	if int64(len(b)) > MaxAttachmentBytes {
		return a, fmt.Errorf("이미지는 파일당 10MB 이하만 허용")
	}
	cfg, format, err := attachmentConfig(b)
	if err != nil {
		return a, fmt.Errorf("이미지 헤더를 읽을 수 없음: %w", err)
	}
	mime := "image/" + format
	if attachmentExt(mime) == "" {
		return a, fmt.Errorf("PNG, JPEG, WEBP만 허용")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return a, fmt.Errorf("이미지 크기가 올바르지 않음")
	}
	a = Attachment{ID: newID(), Name: filepath.Base(strings.ReplaceAll(name, "\\", "/")), Mime: mime, Bytes: int64(len(b)), Width: cfg.Width, Height: cfg.Height, By: by, At: nowMs()}
	if err := os.MkdirAll(s.attachmentDir(roomID), 0o700); err != nil {
		return Attachment{}, err
	}
	path := s.attachmentPath(roomID, a)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return Attachment{}, err
	}
	meta, err := json.Marshal(a)
	if err == nil {
		err = os.WriteFile(filepath.Join(s.attachmentDir(roomID), a.ID+".json"), meta, 0o600)
	}
	if err != nil {
		_ = os.Remove(path)
		return Attachment{}, err
	}
	return a, nil
}

func (s *Store) loadAttachment(roomID, id string) (Attachment, error) {
	var a Attachment
	if !attachmentID.MatchString(id) {
		return a, fmt.Errorf("잘못된 첨부 ID")
	}
	b, err := os.ReadFile(filepath.Join(s.attachmentDir(roomID), id+".json"))
	if err != nil {
		return a, fmt.Errorf("이 방의 첨부를 찾을 수 없음: %s", id)
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return a, err
	}
	if a.ID != id || attachmentExt(a.Mime) == "" {
		return a, fmt.Errorf("첨부 메타데이터 오류: %s", id)
	}
	return a, nil
}

func (h *Hub) UploadAttachment(roomID, name string, source io.Reader) (Attachment, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing || h.rooms[roomID] == nil {
		return Attachment{}, fmt.Errorf("방 없음 또는 서버 종료 중")
	}
	return h.store.saveAttachment(roomID, name, "user", source)
}

func (h *Hub) OpenAttachment(roomID, id string) (*os.File, Attachment, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[roomID] == nil {
		return nil, Attachment{}, fmt.Errorf("방 없음")
	}
	a, err := h.store.loadAttachment(roomID, id)
	if err != nil {
		return nil, a, err
	}
	f, err := os.Open(h.store.attachmentPath(roomID, a))
	return f, a, err
}

func (h *Hub) resolveAttachments(rs *roomState, ids []string) ([]Attachment, error) {
	if len(ids) > MaxAttachments {
		return nil, fmt.Errorf("메시지당 이미지는 4장 이하만 허용")
	}
	used := map[string]bool{}
	for _, m := range rs.msgs {
		for _, a := range m.Attachments {
			used[a.ID] = true
		}
	}
	var out []Attachment
	for _, id := range ids {
		if used[id] {
			return nil, fmt.Errorf("이미 메시지에 붙었거나 중복된 첨부: %s", id)
		}
		a, err := h.store.loadAttachment(rs.room.ID, id)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(h.store.attachmentPath(rs.room.ID, a)); err != nil {
			return nil, err
		}
		out = append(out, a)
		used[id] = true
	}
	return out, nil
}

// Resolve both the candidate and roots before comparing; textual prefixes do
// not contain ../, sibling prefixes or symlinks to files outside these roots.
func attachmentSource(path, workspace, scratch string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, root := range []string{workspace, scratch} {
		if root == "" {
			continue
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		root, err = filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, real)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("허용된 작업 폴더·scratch 밖의 경로")
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("일반 파일만 첨부할 수 있음")
	}
	return f, nil
}

func (s *Store) cleanupAttachments(roomID string, messages []*Message) error {
	used := map[string]bool{}
	for _, m := range messages {
		for _, a := range m.Attachments {
			used[a.ID] = true
		}
	}
	files, err := os.ReadDir(s.attachmentDir(roomID))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, file := range files {
		id := strings.TrimSuffix(file.Name(), filepath.Ext(file.Name()))
		if file.IsDir() || used[id] {
			continue
		}
		info, err := file.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(s.attachmentDir(roomID), file.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Hub) addTurnImages(rs *roomState, p *Participant, history []*Message, opts *agent.RunOptions, reason ...string) {
	added := 0
	for _, m := range history {
		for _, a := range m.Attachments {
			path, _ := filepath.Abs(h.store.attachmentPath(rs.room.ID, a))
			opts.Images = append(opts.Images, agent.Image{Path: path, Mime: a.Mime})
			access := "첨부됨"
			if p.Provider != "codex" {
				access = "경로: " + path
			}
			opts.Prompt += fmt.Sprintf("\n[메시지 %s / %s] [첨부 %s: %s %d×%d %q] %s", m.ID, speaker(m, rs.room), a.ID, a.Mime, a.Width, a.Height, a.Name, access)
			added++
		}
	}
	if added == 0 {
		return
	}
	mode := opts.Mode
	if mode == "" && rs != nil && rs.room != nil {
		mode = rs.room.Mode
	}
	canAttach := mode == agent.ModeRead || mode == agent.ModeWrite
	canRequest := (rs == nil || rs.disc == nil) && !strings.Contains(opts.Prompt, "[[DISCUSSION:CONTINUE]]")
	for _, r := range reason {
		if r == "discussion" || r == "review" {
			canRequest = false
		}
		if r == "handoff" { // markers are not run in a handoff turn
			opts.Prompt += "\n(이미지 첨부 안내: 첨부된 이미지를 직접 확인하고 필요한 내용을 인계 요약에 적으세요.)"
			return
		}
	}
	switch {
	case canAttach && canRequest:
		opts.Prompt += "\n(이미지 첨부 안내: 추가 시각 검토가 필요하면 [[ATTACH:캡처.png]]로 이미지를 공유하거나, 이미지 판독·시각 검토에 강점이 있는 좌석에 [[REQUEST:@좌석ID]]로 시각 검토를 요청하세요.)"
	case canAttach:
		opts.Prompt += "\n(이미지 첨부 안내: 추가 시각 검토 자료가 있으면 [[ATTACH:캡처.png]]로 이미지를 공유하세요. discussion·review 턴에서는 새 요청을 보낼 수 없습니다.)"
	case canRequest:
		opts.Prompt += "\n(이미지 첨부 안내: 추가 시각 검토가 필요하면 이미지 판독·시각 검토에 강점이 있는 좌석에 [[REQUEST:@좌석ID]]로 시각 검토를 요청하세요. chat 모드에서는 파일을 첨부할 수 없습니다.)"
	default:
		opts.Prompt += "\n(이미지 첨부 안내: 첨부된 이미지를 직접 확인해 시각적으로 검토하세요. chat 모드의 discussion·review 턴에서는 파일 첨부나 새 요청을 보낼 수 없습니다.)"
	}
}
