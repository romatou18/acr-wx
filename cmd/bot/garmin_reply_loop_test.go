package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// These tests drive the WHOLE Garmin reply loop — shortlink GET → redirect →
// page parse (Guid/MessageId + sender coords) → JSON "Send" POST — against a fake
// Garmin server. No network, no device, deterministic: runs under `go test ./cmd/bot/`.
//
// They cover the HTTP mechanics that the pure-parser unit tests in parse_test.go
// can't: host/extId derivation from the redirect, the POST URL/headers/JSON body,
// multi-part chunking, and success/failure detection.

// Sanitized test markers — must match cmd/bot/testdata/garmin_message_page.html.
const (
	fixtureGuid      = "TEST-guid-0000-0000-000000000001"
	fixtureMessageID = "999000111"
	fixtureExtID     = "TESTEXT"
	fixtureLat       = -43.730000
	fixtureLon       = 170.090000
)

// capturedPost records one POST the fake Garmin server received.
type capturedPost struct {
	Path   string
	Header http.Header
	Body   garminReplyBody
	Raw    string
}

// fakeGarmin is an httptest TLS server standing in for explore.garmin.com.
type fakeGarmin struct {
	*httptest.Server
	mu       sync.Mutex
	posts    []capturedPost
	respCode int    // POST response status (default 200)
	respBody string // POST response body (default {"Success":true})
}

func (f *fakeGarmin) recorded() []capturedPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedPost(nil), f.posts...)
}

// newFakeGarmin stands up the fake server and points garminHTTPClient at it for
// the duration of the test. Routes:
//
//	GET  /s/{token}           → 302 redirect to /textmessage/txtmsg?extId=TESTEXT
//	GET  /textmessage/txtmsg  → serve the golden message page
//	POST /TextMessage/TxtMsg  → record the request, reply per respCode/respBody
func newFakeGarmin(t *testing.T, pageHTML string) *fakeGarmin {
	t.Helper()
	f := &fakeGarmin{respCode: http.StatusOK, respBody: `{"Success":true}`}

	mux := http.NewServeMux()
	mux.HandleFunc("/s/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/textmessage/txtmsg?extId="+fixtureExtID, http.StatusFound)
	})
	mux.HandleFunc("/textmessage/txtmsg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, pageHTML)
	})
	mux.HandleFunc("/TextMessage/TxtMsg", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body garminReplyBody
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.posts = append(f.posts, capturedPost{
			Path: r.URL.Path, Header: r.Header.Clone(), Body: body, Raw: string(raw),
		})
		code, resp := f.respCode, f.respBody
		f.mu.Unlock()
		w.WriteHeader(code)
		io.WriteString(w, resp)
	})

	f.Server = httptest.NewTLSServer(mux)

	old := garminHTTPClient
	garminHTTPClient = func() *http.Client { return f.Server.Client() }
	t.Cleanup(func() {
		garminHTTPClient = old
		f.Server.Close()
	})
	return f
}

func loadPageFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/garmin_message_page.html")
	if err != nil {
		t.Fatalf("golden page fixture missing: %v", err)
	}
	return string(b)
}

// TestGarminReplyLoop_Success drives the happy path end to end and asserts both
// the parse (host/extId/guid/messageId/coords) and the POST (URL, headers, body).
func TestGarminReplyLoop_Success(t *testing.T) {
	t.Setenv("EMAIL_USER", "bot@example.com") // SendGarminReply uses it as ReplyAddress
	page := loadPageFixture(t)
	f := newFakeGarmin(t, page)

	// 1. Shortlink → page → session (exercises fetchInReachPage + newGarminSessionFromPage).
	sess, err := InitGarminSession(f.URL + "/s/abc123")
	if err != nil {
		t.Fatalf("InitGarminSession: %v", err)
	}
	wantHost := strings.TrimPrefix(f.URL, "https://")
	if sess.Host != wantHost {
		t.Errorf("Host = %q, want %q", sess.Host, wantHost)
	}
	if sess.ExtID != fixtureExtID {
		t.Errorf("ExtID = %q, want %q", sess.ExtID, fixtureExtID)
	}
	if sess.Guid != fixtureGuid || sess.MessageId != fixtureMessageID {
		t.Errorf("session reply target = (%q,%q), want (%q,%q)", sess.Guid, sess.MessageId, fixtureGuid, fixtureMessageID)
	}

	// 2. The page's Locations[] still parses to the sender fix.
	if lat, lon, ok := parseShortlinkCoords(page); !ok || lat != fixtureLat || lon != fixtureLon {
		t.Errorf("parseShortlinkCoords = (%v,%v,%v), want (%v,%v,true)", lat, lon, ok, fixtureLat, fixtureLon)
	}

	// 3. "Click Send": POST the reply and assert what the server received.
	const msg = "YR T:8C D1:0mm | AVL:2-MODR"
	if err := SendGarminReply(sess, msg); err != nil {
		t.Fatalf("SendGarminReply: %v", err)
	}
	posts := f.recorded()
	if len(posts) != 1 {
		t.Fatalf("got %d POSTs, want 1", len(posts))
	}
	p := posts[0]
	if p.Path != "/TextMessage/TxtMsg" {
		t.Errorf("POST path = %q", p.Path)
	}
	if ct := p.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if xrw := p.Header.Get("X-Requested-With"); xrw != "XMLHttpRequest" {
		t.Errorf("X-Requested-With = %q, want XMLHttpRequest", xrw)
	}
	if p.Header.Get("Origin") == "" || p.Header.Get("Referer") == "" {
		t.Errorf("missing Origin/Referer: origin=%q referer=%q", p.Header.Get("Origin"), p.Header.Get("Referer"))
	}
	if p.Body.ReplyMessage != msg {
		t.Errorf("ReplyMessage = %q, want %q", p.Body.ReplyMessage, msg)
	}
	if p.Body.Guid != fixtureGuid || p.Body.MessageId != fixtureMessageID {
		t.Errorf("POST guid/msgId = (%q,%q)", p.Body.Guid, p.Body.MessageId)
	}
	if p.Body.ReplyAddress != "bot@example.com" {
		t.Errorf("ReplyAddress = %q, want bot@example.com", p.Body.ReplyAddress)
	}
}

// TestGarminReplyLoop_Chunking asserts a >160-char message is split into the
// exact chunks splitForGarmin produces, one POST each.
func TestGarminReplyLoop_Chunking(t *testing.T) {
	t.Setenv("EMAIL_USER", "bot@example.com")
	f := newFakeGarmin(t, loadPageFixture(t))
	sess, err := InitGarminSession(f.URL + "/s/abc123")
	if err != nil {
		t.Fatalf("InitGarminSession: %v", err)
	}

	long := "YR T:12C D1:41mm | MS(AOR) D1 Rain w/ heavyFalls S W1k:75 2k:95 3k:95 padding padding padding padding padding | D2 Shwrs then rain W1k:100 2k:110 3k:120 | AVL:4-HIGH"
	want := splitForGarmin(long)
	if len(want) != 2 {
		t.Fatalf("test precondition: expected splitForGarmin to yield 2 chunks, got %d", len(want))
	}
	if err := SendGarminReply(sess, long); err != nil {
		t.Fatalf("SendGarminReply: %v", err)
	}

	posts := f.recorded()
	if len(posts) != len(want) {
		t.Fatalf("got %d POSTs, want %d", len(posts), len(want))
	}
	for i, p := range posts {
		if p.Body.ReplyMessage != want[i] {
			t.Errorf("chunk %d = %q, want %q", i, p.Body.ReplyMessage, want[i])
		}
	}
}

// TestGarminReplyLoop_Failures asserts a 200 is NOT treated as delivery unless the
// body confirms it — {"Success":false} and an HTML error page must both error.
func TestGarminReplyLoop_Failures(t *testing.T) {
	t.Setenv("EMAIL_USER", "bot@example.com")
	cases := map[string]string{
		"success false": `{"Success":false}`,
		"html error":    `<!DOCTYPE html><title>Error</title><body>request blocked</body>`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeGarmin(t, loadPageFixture(t))
			f.mu.Lock()
			f.respBody = body // still HTTP 200
			f.mu.Unlock()

			sess, err := InitGarminSession(f.URL + "/s/abc123")
			if err != nil {
				t.Fatalf("InitGarminSession: %v", err)
			}
			if err := SendGarminReply(sess, "YR T:8C"); err == nil {
				t.Errorf("SendGarminReply returned nil error for non-delivery body %q", body)
			}
		})
	}
}
