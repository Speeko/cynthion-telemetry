package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type commentView struct {
	ID        int64         `json:"id"`
	LevelID   string        `json:"level_id"`
	InstallID string        `json:"install_id"`
	Author    string        `json:"author"`
	Body      string        `json:"body"`
	ReplyTo   *int64        `json:"reply_to"`
	Replies   []commentView `json:"replies"`
}

type commentList struct {
	OK       bool          `json:"ok"`
	Sort     string        `json:"sort"`
	Reason   string        `json:"reason"`
	Comments []commentView `json:"comments"`
	ID       int64         `json:"id"`
	Comment  commentView   `json:"comment"`
	Deleted  bool          `json:"deleted"`
}

type notifView struct {
	ID        int64   `json:"id"`
	Type      string  `json:"type"`
	Kind      string  `json:"kind"`
	Actor     string  `json:"actor_install_id"`
	LevelID   *string `json:"level_id"`
	CommentID *int64  `json:"comment_id"`
	Milestone *int64  `json:"milestone"`
	Read      bool    `json:"read"`
	ReadAt    *int64  `json:"read_at"`
}

type notifList struct {
	OK            bool        `json:"ok"`
	Reason        string      `json:"reason"`
	Notifications []notifView `json:"notifications"`
	Updated       int64       `json:"updated"`
}

func doJSON(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func mustJSONStatus(t *testing.T, status int, raw []byte, want int) {
	t.Helper()
	if status != want {
		t.Fatalf("status=%d want %d body=%s", status, want, raw)
	}
}

func uploadCourse(t *testing.T, ts *httptest.Server, id, install, author string) {
	t.Helper()
	status, raw := postJSON(t, ts.URL+"/v1/manrocket/levels", "", map[string]any{
		"id": id, "title": id, "author": author, "install_id": install,
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
}

func commentsURL(ts *httptest.Server, levelID string) string {
	return ts.URL + "/v1/manrocket/levels/" + levelID + "/comments"
}

func decodeComments(t *testing.T, raw []byte) commentList {
	t.Helper()
	var out commentList
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("comments json: %v body=%s", err, raw)
	}
	return out
}

func decodeNotifs(t *testing.T, raw []byte) notifList {
	t.Helper()
	var out notifList
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("notifications json: %v body=%s", err, raw)
	}
	return out
}

func countNotifs(t *testing.T, s *server, recipient, typ string) int {
	t.Helper()
	var n int
	err := s.store.db.QueryRow(
		`SELECT COUNT(1) FROM manrocket_level_notifications WHERE install_id=? AND kind=?`,
		recipient, typ,
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLevelCommentsCRUDInbox(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	uploadCourse(t, ts, "pad", "owner", "ada")

	// No API key. Author omitted → anon. Level owner is not notified for their own comment.
	status, raw := doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "owner",
		"body":       "  my pad  ",
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	own := decodeComments(t, raw)
	if !own.OK || own.ID == 0 || own.Comment.Author != "anon" || own.Comment.Body != "my pad" || own.Comment.ReplyTo != nil {
		t.Fatalf("own comment: %+v", own)
	}
	if countNotifs(t, s, "owner", notifCommentOnLevel) != 0 {
		t.Fatal("owner should not be notified about their own comment")
	}

	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-b",
		"author":     "bea",
		"body":       "first",
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	first := decodeComments(t, raw)
	if first.Comment.Author != "bea" || first.Comment.InstallID != "pilot-b" || first.Comment.LevelID != "pad" {
		t.Fatalf("first comment: %+v", first.Comment)
	}

	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-c",
		"author":     "cy",
		"body":       "second",
		"reply_to":   first.ID,
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	second := decodeComments(t, raw)
	if second.Comment.ReplyTo == nil || *second.Comment.ReplyTo != first.ID {
		t.Fatalf("reply_to: %+v", second.Comment)
	}

	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-b",
		"author":     "bea",
		"body":       "third",
		"reply_to":   second.ID,
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	third := decodeComments(t, raw)

	if countNotifs(t, s, "owner", notifCommentOnLevel) != 3 {
		t.Fatalf("level owner comment notifications = %d", countNotifs(t, s, "owner", notifCommentOnLevel))
	}
	if countNotifs(t, s, "pilot-b", notifReplyToYou) != 1 {
		t.Fatalf("pilot-b replies = %d", countNotifs(t, s, "pilot-b", notifReplyToYou))
	}
	if countNotifs(t, s, "pilot-c", notifReplyToYou) != 1 {
		t.Fatalf("pilot-c replies = %d", countNotifs(t, s, "pilot-c", notifReplyToYou))
	}
	if countTable(t, s, "manrocket_events")+countTable(t, s, "events") != 0 {
		t.Fatal("comments wrote an analytics event")
	}

	status, raw = doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/notifications?install_id=pilot-b", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	inbox := decodeNotifs(t, raw)
	if len(inbox.Notifications) != 1 || inbox.Notifications[0].Type != notifReplyToYou || inbox.Notifications[0].Read {
		t.Fatalf("pilot-b inbox: %+v", inbox.Notifications)
	}
	if inbox.Notifications[0].CommentID == nil || *inbox.Notifications[0].CommentID != second.ID {
		t.Fatalf("reply notification comment_id: %+v", inbox.Notifications[0])
	}
	if inbox.Notifications[0].Actor != "pilot-c" {
		t.Fatalf("reply actor: %+v", inbox.Notifications[0])
	}

	status, raw = doJSON(t, http.MethodGet, commentsURL(ts, "pad")+"?sort=new", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	listed := decodeComments(t, raw)
	if listed.Sort != "new" || len(listed.Comments) != 4 {
		t.Fatalf("newest list: %+v", listed)
	}
	if listed.Comments[0].ID != third.ID || listed.Comments[1].ID != second.ID || listed.Comments[2].ID != first.ID {
		t.Fatalf("newest order: %+v", listed.Comments)
	}

	status, raw = doJSON(t, http.MethodGet, fmt.Sprintf("%s?sort=thread", commentsURL(ts, "pad")), nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	threaded := decodeComments(t, raw)
	if threaded.Sort != "thread" || len(threaded.Comments) != 2 {
		t.Fatalf("thread roots: %+v", threaded)
	}
	// owner's comment is a root; "first" is the other root and holds the chain
	var chain commentView
	foundChain := false
	for _, c := range threaded.Comments {
		if c.ID == first.ID {
			chain = c
			foundChain = true
		}
		if c.Replies == nil {
			t.Fatalf("thread comment %d missing replies array", c.ID)
		}
	}
	if !foundChain || len(chain.Replies) != 1 || chain.Replies[0].ID != second.ID {
		t.Fatalf("thread chain: %+v", chain)
	}
	if len(chain.Replies[0].Replies) != 1 || chain.Replies[0].Replies[0].ID != third.ID {
		t.Fatalf("nested reply: %+v", chain.Replies)
	}

	status, raw = doJSON(t, http.MethodGet, fmt.Sprintf("%s?parent_id=%d", commentsURL(ts, "pad"), first.ID), nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	children := decodeComments(t, raw)
	if len(children.Comments) != 1 || children.Comments[0].ID != second.ID {
		t.Fatalf("parent_id filter: %+v", children.Comments)
	}
	status, raw = doJSON(t, http.MethodGet, fmt.Sprintf("%s?reply_to=%d&sort=new", commentsURL(ts, "pad"), first.ID), nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	children = decodeComments(t, raw)
	if len(children.Comments) != 1 || children.Comments[0].ID != second.ID {
		t.Fatalf("reply_to filter: %+v", children.Comments)
	}

	// Stranger cannot delete or edit.
	status, raw = doJSON(t, http.MethodDelete, fmt.Sprintf("%s/%d", commentsURL(ts, "pad"), first.ID), map[string]any{
		"install_id": "pilot-c",
	})
	mustJSONStatus(t, status, raw, http.StatusForbidden)
	status, raw = doJSON(t, http.MethodPatch, fmt.Sprintf("%s/%d", commentsURL(ts, "pad"), first.ID), map[string]any{
		"install_id": "pilot-c",
		"body":       "nope",
	})
	mustJSONStatus(t, status, raw, http.StatusForbidden)

	status, raw = doJSON(t, http.MethodPatch, fmt.Sprintf("%s/%d", commentsURL(ts, "pad"), first.ID), map[string]any{
		"install_id": "pilot-b",
		"body":       "first, edited",
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	edited := decodeComments(t, raw)
	if edited.Comment.Body != "first, edited" || edited.Comment.Author != "bea" {
		t.Fatalf("patch: %+v", edited.Comment)
	}

	status, raw = doJSON(t, http.MethodDelete, fmt.Sprintf("%s/%d?install_id=pilot-b", commentsURL(ts, "pad"), first.ID), nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	if !decodeComments(t, raw).Deleted {
		t.Fatalf("delete: %s", raw)
	}
	var deletedAt sql.NullInt64
	var keptBody string
	if err := s.store.db.QueryRow(`SELECT deleted_at, body FROM manrocket_level_comments WHERE id=?`, first.ID).Scan(&deletedAt, &keptBody); err != nil {
		t.Fatal(err)
	}
	if !deletedAt.Valid || keptBody != "first, edited" {
		t.Fatalf("soft-delete deleted_at=%v body=%q", deletedAt, keptBody)
	}

	status, raw = doJSON(t, http.MethodGet, commentsURL(ts, "pad")+"?sort=thread", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	after := decodeComments(t, raw)
	for _, c := range after.Comments {
		if c.ID == first.ID {
			t.Fatal("soft-deleted comment still listed")
		}
	}
	// The reply survives, promoted because its parent is gone.
	var promoted commentView
	foundPromoted := false
	for _, c := range after.Comments {
		if c.ID == second.ID {
			promoted = c
			foundPromoted = true
		}
	}
	if !foundPromoted || len(promoted.Replies) != 1 || promoted.Replies[0].ID != third.ID {
		t.Fatalf("promoted thread: %+v", after.Comments)
	}

	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-c",
		"author":     "cy",
		"body":       "reply to deleted",
		"reply_to":   first.ID,
	})
	mustJSONStatus(t, status, raw, http.StatusBadRequest)

	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-b",
		"author":     "bea",
		"body":       "gone soon",
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	extra := decodeComments(t, raw)
	status, raw = doJSON(t, http.MethodPost, fmt.Sprintf("%s/%d/delete", commentsURL(ts, "pad"), extra.ID), map[string]any{
		"install_id": "pilot-b",
	})
	mustJSONStatus(t, status, raw, http.StatusOK)

	status, raw = doJSON(t, http.MethodGet, commentsURL(ts, "pad")+"?sort=new", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	surviving := decodeComments(t, raw)
	for _, c := range surviving.Comments {
		if c.ID == extra.ID || c.ID == first.ID {
			t.Fatalf("deleted comment %d still listed", c.ID)
		}
	}

	var ip string
	if err := s.store.db.QueryRow(`SELECT ip FROM manrocket_level_comments WHERE id=?`, first.ID).Scan(&ip); err != nil {
		t.Fatal(err)
	}
	if len(ip) != 16 {
		t.Fatalf("ip tag = %q", ip)
	}
}

func TestCommentValidation(t *testing.T) {
	_, ts := newIngestTestServer(t, testManRocketKey)
	uploadCourse(t, ts, "pad", "owner", "ada")
	uploadCourse(t, ts, "other", "owner", "ada")

	status, raw := doJSON(t, http.MethodPost, commentsURL(ts, "missing"), map[string]any{
		"install_id": "pilot-b",
		"body":       "hi",
	})
	mustJSONStatus(t, status, raw, http.StatusNotFound)

	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-b",
		"body":       "   \n",
	})
	mustJSONStatus(t, status, raw, http.StatusBadRequest)
	if decodeComments(t, raw).Reason != "empty" {
		t.Fatalf("empty reason: %s", raw)
	}

	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "",
		"body":       "hi",
	})
	mustJSONStatus(t, status, raw, http.StatusBadRequest)

	long := strings.Repeat("ä", commentMaxBodyRunes+1)
	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-b",
		"body":       long,
	})
	mustJSONStatus(t, status, raw, http.StatusBadRequest)
	if decodeComments(t, raw).Reason != "too long" {
		t.Fatalf("too long: %s", raw)
	}
	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-b",
		"author":     "bea",
		"body":       strings.Repeat("ä", commentMaxBodyRunes),
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	parent := decodeComments(t, raw).ID

	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "other"), map[string]any{
		"install_id": "pilot-c",
		"body":       "cross level",
		"reply_to":   parent,
	})
	mustJSONStatus(t, status, raw, http.StatusBadRequest)
	if decodeComments(t, raw).Reason != "bad reply_to" {
		t.Fatalf("cross-level reply: %s", raw)
	}

	status, raw = doJSON(t, http.MethodGet, commentsURL(ts, "pad")+"?sort=sideways", nil)
	mustJSONStatus(t, status, raw, http.StatusBadRequest)
	status, raw = doJSON(t, http.MethodGet, commentsURL(ts, "pad")+"?reply_to=nope", nil)
	mustJSONStatus(t, status, raw, http.StatusBadRequest)
	status, raw = doJSON(t, http.MethodGet, commentsURL(ts, "pad")+"?limit=0", nil)
	mustJSONStatus(t, status, raw, http.StatusBadRequest)

	status, raw = doJSON(t, http.MethodPut, commentsURL(ts, "pad"), map[string]any{"install_id": "pilot-b", "body": "x"})
	mustJSONStatus(t, status, raw, http.StatusMethodNotAllowed)

	// Per-install daily cap. A second install on the same IP is still under the IP cap.
	okN := 1 // the 500-rune comment above
	for okN < commentDailyInstall {
		status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
			"install_id": "pilot-b",
			"body":       fmt.Sprintf("n-%d", okN),
		})
		mustJSONStatus(t, status, raw, http.StatusOK)
		okN++
	}
	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-b",
		"body":       "over the cap",
	})
	mustJSONStatus(t, status, raw, http.StatusTooManyRequests)
	if decodeComments(t, raw).Reason != "daily comment limit reached" {
		t.Fatalf("cap reason: %s", raw)
	}
	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-c",
		"body":       "other install",
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
}

func TestVoteNotifiesLevelOwnerOnce(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	uploadCourse(t, ts, "pad", "owner", "ada")
	uploadCourse(t, ts, "solo", "owner", "ada")

	vote := func(level, install string, value int) int {
		t.Helper()
		status, raw := postJSON(t, ts.URL+"/v1/manrocket/levels/"+level+"/vote", "", map[string]any{
			"install_id": install,
			"value":      value,
		})
		mustJSONStatus(t, status, raw, http.StatusOK)
		var out struct {
			OK    bool `json:"ok"`
			Votes int  `json:"votes"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || !out.OK {
			t.Fatalf("vote body: %s", raw)
		}
		return out.Votes
	}

	if got := vote("pad", "pilot-b", -1); got != -1 {
		t.Fatalf("downvote votes=%d", got)
	}
	if countNotifs(t, s, "owner", notifVoteOnLevel) != 0 {
		t.Fatal("downvote wrote a like notification")
	}
	if got := vote("pad", "pilot-b", 1); got != 1 {
		t.Fatalf("upvote votes=%d", got)
	}
	if countNotifs(t, s, "owner", notifVoteOnLevel) != 1 {
		t.Fatalf("upvote notifications=%d", countNotifs(t, s, "owner", notifVoteOnLevel))
	}
	if got := vote("pad", "pilot-b", 1); got != 1 {
		t.Fatalf("repeat upvote votes=%d", got)
	}
	if got := vote("pad", "pilot-b", -1); got != -1 {
		t.Fatalf("flip down votes=%d", got)
	}
	if got := vote("pad", "pilot-b", 1); got != 1 {
		t.Fatalf("flip up votes=%d", got)
	}
	if countNotifs(t, s, "owner", notifVoteOnLevel) != 1 {
		t.Fatalf("repeat +1 stacked notifications: %d", countNotifs(t, s, "owner", notifVoteOnLevel))
	}
	if got := vote("pad", "pilot-c", 1); got != 2 {
		t.Fatalf("second voter votes=%d", got)
	}
	if countNotifs(t, s, "owner", notifVoteOnLevel) != 2 {
		t.Fatalf("second voter notifications=%d", countNotifs(t, s, "owner", notifVoteOnLevel))
	}

	status, raw := doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/notifications?install_id=owner&unread=1", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	inbox := decodeNotifs(t, raw)
	if len(inbox.Notifications) != 2 {
		t.Fatalf("vote inbox: %+v", inbox.Notifications)
	}
	for _, n := range inbox.Notifications {
		if n.Type != notifVoteOnLevel || n.CommentID != nil || n.Milestone != nil || n.LevelID == nil || *n.LevelID != "pad" {
			t.Fatalf("vote notification shape: %+v", n)
		}
	}

	if got := vote("solo", "owner", 1); got != 1 {
		t.Fatalf("self vote votes=%d", got)
	}
	if countNotifs(t, s, "owner", notifVoteOnLevel) != 2 {
		t.Fatal("self-vote wrote a notification")
	}

	status, raw = postJSON(t, ts.URL+"/v1/manrocket/levels/missing/vote", "", map[string]any{
		"install_id": "pilot-b",
		"value":      1,
	})
	mustJSONStatus(t, status, raw, http.StatusNotFound)

	// Level list/get still respond, and the vote total is the one we just built.
	status, raw = doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/levels/pad", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	var gotLevel struct {
		OK    bool `json:"ok"`
		Votes int  `json:"votes"`
	}
	if err := json.Unmarshal(raw, &gotLevel); err != nil || !gotLevel.OK || gotLevel.Votes != 2 {
		t.Fatalf("level get: %s", raw)
	}
}

func TestNotificationRead(t *testing.T) {
	_, ts := newIngestTestServer(t, testManRocketKey)
	uploadCourse(t, ts, "pad", "owner", "ada")
	status, raw := doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-b",
		"author":     "bea",
		"body":       "hello",
	})
	mustJSONStatus(t, status, raw, http.StatusOK)

	status, raw = doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/notifications?install_id=owner&unread=true", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	inbox := decodeNotifs(t, raw)
	if len(inbox.Notifications) != 1 || inbox.Notifications[0].Type != notifCommentOnLevel || inbox.Notifications[0].Read {
		t.Fatalf("unread inbox: %+v", inbox.Notifications)
	}
	nid := inbox.Notifications[0].ID

	status, raw = doJSON(t, http.MethodPost, ts.URL+"/v1/manrocket/notifications/read", map[string]any{
		"install_id": "someone-else",
		"ids":        []int64{nid},
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	if decodeNotifs(t, raw).Updated != 0 {
		t.Fatalf("stranger marked read: %s", raw)
	}

	status, raw = doJSON(t, http.MethodPost, ts.URL+"/v1/manrocket/notifications/read", map[string]any{
		"install_id": "owner",
		"ids":        []int64{nid},
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	if decodeNotifs(t, raw).Updated != 1 {
		t.Fatalf("mark one: %s", raw)
	}
	status, raw = doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/notifications?install_id=owner&unread=1", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	if len(decodeNotifs(t, raw).Notifications) != 0 {
		t.Fatalf("still unread: %s", raw)
	}
	status, raw = doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/notifications?install_id=owner", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	all := decodeNotifs(t, raw)
	if len(all.Notifications) != 1 || !all.Notifications[0].Read || all.Notifications[0].ReadAt == nil {
		t.Fatalf("read row: %+v", all.Notifications)
	}

	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-c",
		"body":       "another",
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	status, raw = doJSON(t, http.MethodPost, ts.URL+"/v1/manrocket/notifications/read", map[string]any{
		"install_id": "owner",
		"all":        true,
		"ids":        []int64{999999},
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	if decodeNotifs(t, raw).Updated != 1 {
		t.Fatalf("mark all: %s", raw)
	}
	status, raw = doJSON(t, http.MethodPost, ts.URL+"/v1/manrocket/notifications/read", map[string]any{
		"install_id": "owner",
		"all":        true,
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	if decodeNotifs(t, raw).Updated != 0 {
		t.Fatalf("mark all again: %s", raw)
	}

	status, raw = doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/notifications", nil)
	mustJSONStatus(t, status, raw, http.StatusBadRequest)
	status, raw = doJSON(t, http.MethodPost, ts.URL+"/v1/manrocket/notifications/read", map[string]any{
		"install_id": "owner",
	})
	mustJSONStatus(t, status, raw, http.StatusBadRequest)
	status, raw = doJSON(t, http.MethodPost, ts.URL+"/v1/manrocket/notifications", map[string]any{"install_id": "owner"})
	mustJSONStatus(t, status, raw, http.StatusMethodNotAllowed)
}

func TestPlayMilestoneShowsInInbox(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	uploadCourse(t, ts, "pad", "owner", "ada")

	status, _, raw := playMR(t, ts.URL, "pad", "complete", "pilot", "sess-c")
	mustJSONStatus(t, status, raw, http.StatusOK)
	if countNotifs(t, s, "owner", notifPlayMilestone) != 0 {
		t.Fatal("completion wrote a play milestone")
	}

	status, body, raw := playMR(t, ts.URL, "pad", "start", "pilot", "sess-1")
	mustJSONStatus(t, status, raw, http.StatusOK)
	if body["counted"] != true || body["play_count"] != float64(1) {
		t.Fatalf("first start: %s", raw)
	}
	status, body, raw = playMR(t, ts.URL, "pad", "start", "pilot", "sess-1")
	mustJSONStatus(t, status, raw, http.StatusOK)
	if body["counted"] != false || body["play_count"] != float64(1) {
		t.Fatalf("retry: %s", raw)
	}
	if countNotifs(t, s, "owner", notifPlayMilestone) != 1 {
		t.Fatalf("milestone rows=%d", countNotifs(t, s, "owner", notifPlayMilestone))
	}

	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET play_count=9 WHERE id='pad'`); err != nil {
		t.Fatal(err)
	}
	status, _, raw = playMR(t, ts.URL, "pad", "start", "pilot", "sess-10")
	mustJSONStatus(t, status, raw, http.StatusOK)
	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET play_count=99 WHERE id='pad'`); err != nil {
		t.Fatal(err)
	}
	status, _, raw = playMR(t, ts.URL, "pad", "start", "pilot", "sess-100")
	mustJSONStatus(t, status, raw, http.StatusOK)
	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET play_count=499 WHERE id='pad'`); err != nil {
		t.Fatal(err)
	}
	status, _, raw = playMR(t, ts.URL, "pad", "start", "pilot", "sess-500")
	mustJSONStatus(t, status, raw, http.StatusOK)
	if countNotifs(t, s, "owner", notifPlayMilestone) != 4 {
		t.Fatalf("milestone rows=%d", countNotifs(t, s, "owner", notifPlayMilestone))
	}

	status, raw = doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/notifications?install_id=owner&unread=yes", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	inbox := decodeNotifs(t, raw)
	if len(inbox.Notifications) != 4 {
		t.Fatalf("milestone inbox: %+v", inbox.Notifications)
	}
	gotMiles := map[int64]bool{}
	for _, item := range inbox.Notifications {
		if item.Type != notifPlayMilestone || item.Kind != notifPlayMilestone || item.Milestone == nil || item.Actor != "" {
			t.Fatalf("milestone row: %+v", item)
		}
		if item.LevelID == nil || *item.LevelID != "pad" || item.CommentID != nil {
			t.Fatalf("milestone row: %+v", item)
		}
		gotMiles[*item.Milestone] = true
	}
	for _, m := range mrPlayMilestoneThresholds {
		if !gotMiles[int64(m)] {
			t.Fatalf("missing milestone %d in %+v", m, inbox.Notifications)
		}
	}

	status, raw = doJSON(t, http.MethodPost, ts.URL+"/v1/manrocket/notifications/read", map[string]any{
		"install_id": "owner",
		"all":        true,
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	if decodeNotifs(t, raw).Updated != 4 {
		t.Fatalf("mark play rows: %s", raw)
	}
	status, raw = doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/notifications?install_id=owner&unread=1", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	if len(decodeNotifs(t, raw).Notifications) != 0 {
		t.Fatalf("play rows still unread: %s", raw)
	}
}

func TestInboxReadsLegacyPlayMilestoneRow(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "events.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(`CREATE TABLE manrocket_level_notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		level_id TEXT NOT NULL,
		install_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		threshold INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE (level_id, kind, threshold)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.Exec(`INSERT INTO manrocket_level_notifications
		(level_id, install_id, kind, threshold, created_at)
		VALUES ('pad', 'owner-a', 'play_milestone', 10, 111)`); err != nil {
		t.Fatal(err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	current := store
	t.Cleanup(func() {
		if current != nil {
			current.Close()
		}
	})
	s := &server{
		cfg:             Config{APIKey: testCynthionKey, ManRocketAPIKey: testManRocketKey},
		store:           store,
		limiter:         newIPLimiter(1000, 10000),
		bugReportDir:    filepath.Join(dir, "bugreports"),
		manrocketBugDir: filepath.Join(dir, "manrocket_bugreports"),
	}
	ts := httptest.NewServer(s.handler())
	tsClosed := false
	t.Cleanup(func() {
		if !tsClosed {
			ts.Close()
		}
	})

	status, raw := doJSON(t, http.MethodGet, ts.URL+"/v1/manrocket/notifications?install_id=owner-a&unread=1", nil)
	mustJSONStatus(t, status, raw, http.StatusOK)
	inbox := decodeNotifs(t, raw)
	if len(inbox.Notifications) != 1 {
		t.Fatalf("legacy inbox: %+v", inbox.Notifications)
	}
	stub := inbox.Notifications[0]
	if stub.Type != notifPlayMilestone || stub.Kind != notifPlayMilestone || stub.Milestone == nil || *stub.Milestone != 10 || stub.Read || stub.Actor != "" {
		t.Fatalf("legacy stub: %+v", stub)
	}

	uploadCourse(t, ts, "pad", "owner-a", "ada")
	status, raw = doJSON(t, http.MethodPost, commentsURL(ts, "pad"), map[string]any{
		"install_id": "pilot-b",
		"author":     "bea",
		"body":       "on a played level",
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	if countNotifs(t, s, "owner-a", notifPlayMilestone) != 1 {
		t.Fatal("comment disturbed the play milestone row")
	}
	if countNotifs(t, s, "owner-a", notifCommentOnLevel) != 1 {
		t.Fatal("comment notification missing beside the stub")
	}

	res, err := store.db.Exec(`INSERT INTO manrocket_level_notifications
		(level_id, install_id, kind, threshold, created_at)
		VALUES ('pad', 'owner-a', 'play_milestone', 10, 222)
		ON CONFLICT(level_id, kind, threshold) WHERE kind = 'play_milestone' DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	aff, err := res.RowsAffected()
	if err != nil || aff != 0 || countNotifs(t, s, "owner-a", notifPlayMilestone) != 1 {
		t.Fatalf("duplicate milestone aff=%d err=%v count=%d", aff, err, countNotifs(t, s, "owner-a", notifPlayMilestone))
	}

	status, raw = doJSON(t, http.MethodPost, ts.URL+"/v1/manrocket/notifications/read", map[string]any{
		"install_id": "owner-a",
		"ids":        []int64{stub.ID},
	})
	mustJSONStatus(t, status, raw, http.StatusOK)
	if decodeNotifs(t, raw).Updated != 1 {
		t.Fatalf("mark stub: %s", raw)
	}

	tsClosed = true
	ts.Close()
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	current = nil
	reopened, err := openStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	current = reopened
	var readAt sql.NullInt64
	var n int
	if err := reopened.db.QueryRow(
		`SELECT read_at, (SELECT COUNT(1) FROM manrocket_level_notifications WHERE install_id='owner-a' AND kind='play_milestone')
		 FROM manrocket_level_notifications WHERE id=?`, stub.ID,
	).Scan(&readAt, &n); err != nil {
		t.Fatal(err)
	}
	if !readAt.Valid || n != 1 {
		t.Fatalf("reopen read_at=%v milestone rows=%d", readAt, n)
	}
}

func TestCommentCORSAllowsPatchDelete(t *testing.T) {
	_, ts := newIngestTestServer(t, testManRocketKey)
	req, err := http.NewRequest(http.MethodOptions, commentsURL(ts, "pad"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:8080")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("options status=%d", resp.StatusCode)
	}
	allow := resp.Header.Get("Access-Control-Allow-Methods")
	if !strings.Contains(allow, "PATCH") || !strings.Contains(allow, "DELETE") {
		t.Fatalf("allow-methods %q", allow)
	}
}
