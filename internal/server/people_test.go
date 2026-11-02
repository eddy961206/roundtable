package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

type peopleClient struct {
	t      *testing.T
	c      *Community
	cookie string
}

func (pc *peopleClient) do(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	pc.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.168.0.7:5000"
	req.Host = "host:8790"
	if pc.cookie != "" {
		req.AddCookie(&http.Cookie{Name: personCookie, Value: pc.cookie})
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	pc.c.ServeHTTP(w, req)
	return w
}

func (pc *peopleClient) login(token string) *httptest.ResponseRecorder {
	w := pc.do("POST", "/people/login", `{"token":"`+token+`"}`, personHeader, "1")
	for _, c := range w.Result().Cookies() {
		if c.Name == personCookie {
			pc.cookie = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				pc.t.Fatalf("session cookie must be HttpOnly and SameSite=Strict: %+v", c)
			}
		}
	}
	return w
}

// People sign in with an invite token and see community spaces only; a viewer
// watches, a member also sends and approves, and a revoked person is out at once.
func TestPeopleAccess(t *testing.T) {
	store, _ := room.NewStore(t.TempDir())
	hub, err := room.NewHub(store, agent.NewRegistry(markerProvider{}), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Shutdown(context.Background()) })
	space, err := hub.CreateRoom(room.RoomSpec{Title: "작업실", Kind: room.KindWork, Mode: agent.ModeChat, Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	private, err := hub.CreateRoom(room.RoomSpec{Title: "개인 방", Workspace: t.TempDir(), Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	_, viewerToken, err := hub.InvitePerson("구경꾼", room.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	member, memberToken, err := hub.InvitePerson("부장님", room.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := hub.InvitePerson("부장님", room.RoleViewer); err == nil {
		t.Fatal("a second person with the same name was invited")
	}
	allow, _ := ParseAllow(DefaultCommunityAllow)
	c := NewCommunity(hub, allow, fstest.MapFS{"people.html": {Data: []byte("<html>people</html>")}})

	anon := &peopleClient{t: t, c: c}
	if w := anon.do("GET", "/", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "people") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("people page %d %q %v", w.Code, w.Body, w.Header())
	}
	if w := anon.do("GET", "/people/api/spaces", ""); w.Code != 401 {
		t.Fatalf("signed out: %d", w.Code)
	}
	if w := anon.do("POST", "/people/login", `{"token":"`+memberToken+`"}`); w.Code != 403 {
		t.Fatalf("a login without the page header: %d", w.Code)
	}
	if w := anon.login("rtp_wrong"); w.Code != 401 || anon.cookie != "" {
		t.Fatalf("a wrong token: %d", w.Code)
	}

	viewer := &peopleClient{t: t, c: c}
	if w := viewer.login(viewerToken); w.Code != 200 {
		t.Fatal(w.Body)
	}
	w := viewer.do("GET", "/people/api/spaces", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), space.ID) || strings.Contains(w.Body.String(), private.ID) || strings.Contains(w.Body.String(), "workspace") {
		t.Fatalf("spaces only, without host folders: %s", w.Body)
	}
	if w := viewer.do("GET", "/people/api/spaces/"+private.ID, ""); w.Code != 404 {
		t.Fatalf("an ordinary room: %d", w.Code)
	}
	if w := viewer.do("POST", "/people/api/spaces/"+space.ID+"/messages", `{"text":"안녕"}`, personHeader, "1"); w.Code != 403 {
		t.Fatalf("a viewer sent a message: %d", w.Code)
	}

	boss := &peopleClient{t: t, c: c}
	boss.login(memberToken)
	if w := boss.do("POST", "/people/api/spaces/"+space.ID+"/messages", `{"text":"안녕"}`); w.Code != 403 {
		t.Fatalf("a state change without the page header: %d", w.Code)
	}
	if w := boss.do("POST", "/people/api/spaces/"+space.ID+"/messages", `{"text":"안녕"}`, personHeader, "1", "Origin", "http://evil.example"); w.Code != 403 {
		t.Fatalf("a cross-site state change: %d", w.Code)
	}
	if w := boss.do("POST", "/people/api/spaces/"+space.ID+"/messages", `{"text":"녹화 보존 검토 부탁해요","target":"claude"}`, personHeader, "1", "Origin", "http://host:8790"); w.Code != 201 {
		t.Fatalf("a member's message: %d %s", w.Code, w.Body)
	}
	var snap struct {
		Room struct {
			Participants []map[string]any `json:"participants"`
		} `json:"room"`
		Messages []struct {
			Kind, By, Text string
		} `json:"messages"`
	}
	waitSpace := func() string {
		w := boss.do("GET", "/people/api/spaces/"+space.ID, "")
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		return w.Body.String()
	}
	body := waitSpace()
	for _, hidden := range []string{"session_id", "workspace", "usage", "input_all"} {
		if strings.Contains(body, `"`+hidden+`"`) {
			t.Fatalf("a space must not show %s: %s", hidden, body)
		}
	}
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Messages[0].By != "부장님" {
		t.Fatalf("the message must name its person: %+v", snap.Messages)
	}

	var card room.Decision
	for i := 0; i < 300 && card.ID == ""; i++ { // the seat answers the message above with a decision marker
		list, _ := hub.Decisions(space.ID)
		for _, d := range list {
			card = d
		}
		time.Sleep(10 * time.Millisecond)
	}
	if card.Status != "proposed" {
		t.Fatalf("the seat's card: %+v", card)
	}
	if w := viewer.do("PATCH", "/people/api/spaces/"+space.ID+"/decisions/"+card.ID, `{"status":"active"}`, personHeader, "1"); w.Code != 403 {
		t.Fatalf("a viewer approved: %d", w.Code)
	}
	if w := boss.do("PATCH", "/people/api/spaces/"+space.ID+"/decisions/"+card.ID, `{"status":"active","text":"바꾼 결정"}`, personHeader, "1"); w.Code != 200 || !strings.Contains(w.Body.String(), `"decided_by":"부장님"`) || strings.Contains(w.Body.String(), "바꾼 결정") {
		t.Fatalf("a member approves, named, without editing the card: %d %s", w.Code, w.Body)
	}
	if w := boss.do("PATCH", "/people/api/spaces/"+space.ID+"/decisions/"+card.ID, `{"status":"dropped"}`, personHeader, "1"); w.Code != 409 {
		t.Fatalf("a decided card cannot be decided again: %d %s", w.Code, w.Body)
	}

	if err := hub.RevokePerson(member.ID); err != nil {
		t.Fatal(err)
	}
	if w := boss.do("GET", "/people/api/spaces", ""); w.Code != 401 {
		t.Fatalf("a revoked person's session must end: %d", w.Code)
	}
}

// markerProvider is a seat that proposes a decision card in every answer.
type markerProvider struct{ echoProvider }

func (markerProvider) Run(context.Context, agent.RunOptions, func(agent.Event)) (agent.Result, error) {
	text := "검토했어요.\n[[DECISION: 일반 구간 7일 보존 | 근거: #1 | 반대: 없음 | 미확인: 없음]]"
	return agent.Result{Text: text, SessionID: "s"}, nil
}
