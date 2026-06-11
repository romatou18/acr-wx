package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/smtp"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "time/tzdata"

	"acr-wx/internal/forecast"
	"acr-wx/internal/garmin"

	"github.com/PuerkitoBio/goquery"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-message/mail"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// ==========================================
// CONFIGURATION & STRUCTS
// ==========================================
type SessionState struct {
	ExtID         string
	GUID          string
	MessageId     string // Garmin MessageId of the last inbound message (reply target)
	Host          string // regional explore.garmin.com host, e.g. "aus.explore.garmin.com"
	Active        bool
	Lat           float64
	Lon           float64
	Alt           int
	Park          string
	LastFetch     int64
	LastRoutineNZ string
}

// GarminSession holds everything needed to POST a reply back to a device via
// the regional explore.garmin.com TextMessage API. Modern Garmin pages no
// longer use a hidden __RequestVerificationToken form POST — the reply is a
// plain JSON POST to /TextMessage/TxtMsg carrying the message's Guid and
// MessageId (both scraped from hidden inputs on the message page). That
// unguessable Guid/MessageId pair authorises the POST; no session cookie is
// required (confirmed from a captured browser send).
type GarminSession struct {
	Client    *http.Client
	Host      string // regional host, e.g. "aus.explore.garmin.com"
	ExtID     string
	Guid      string
	MessageId string
	ReplyURL  string // the message page URL, used as the POST Referer
}

// inReachPage is the result of following an inreachlink.com shortlink (or a
// reconstructed message-page URL) to the regional explore.garmin.com host.
type inReachPage struct {
	HTML     string
	FinalURL string // the page we landed on (used as the POST Referer)
	Host     string // regional host extracted from FinalURL
	ExtID    string // extId query param from FinalURL
}

// browserUA is sent on every Garmin request so we look like a normal desktop
// browser (Garmin fronts these endpoints with an anti-bot/WAF layer).
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// garminHTTPClient builds the http.Client used for all Garmin GET/POST traffic.
// Overridable in tests to target an httptest server. No cookie jar is required for
// the reply POST (GARMIN.md §4), but we keep one for the shortlink GET/redirect.
var garminHTTPClient = func() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 15 * time.Second}
}

// fetchInReachPage GETs the message page behind an inreachlink.com shortlink,
// following the redirect to the regional explore.garmin.com host. The returned
// page carries everything the rest of the flow needs: the sender's location
// (Locations[] JSON) and the Guid/MessageId hidden inputs required to post a
// reply. A shortlink appears to be single-use, so we fetch the page exactly
// once and reuse it for both purposes.
func fetchInReachPage(shortlink string) (*inReachPage, error) {
	client := garminHTTPClient()
	req, err := http.NewRequest("GET", shortlink, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	log.Printf("🔗 Garmin session: GET %s", shortlink)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to GET inreach URL: %v", err)
	}
	defer resp.Body.Close()

	final := resp.Request.URL
	log.Printf("🔗 Garmin redirect landed on: %s (HTTP %d)", final.String(), resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("inreach page returned HTTP %d (%s) — shortlink may be expired/single-use",
			resp.StatusCode, final.String())
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return &inReachPage{
		HTML:     string(b),
		FinalURL: final.String(),
		Host:     final.Host,
		ExtID:    final.Query().Get("extId"),
	}, nil
}

// parseGarminReplyFields pulls the reply target out of the message page. Garmin
// exposes it as hidden inputs: <input name="Guid" value="..."> and
// <input name="MessageId" value="...">. The implementation lives in
// internal/garmin so the weather-api parse-test endpoint exercises the same code.
func parseGarminReplyFields(html string) (guid, messageId string) {
	return garmin.ParseReplyFields(html)
}

// newGarminSessionFromPage builds a reply session from an already-fetched
// message page. Returns an error if the page didn't carry the Guid/MessageId
// needed to post a reply (link expired, or Garmin changed the page layout).
func newGarminSessionFromPage(p *inReachPage) (*GarminSession, error) {
	guid, messageId := parseGarminReplyFields(p.HTML)
	if guid == "" || messageId == "" {
		return nil, fmt.Errorf("reply fields not found on Garmin page (guidSet=%v msgIdSet=%v) at %s",
			guid != "", messageId != "", p.FinalURL)
	}
	return &GarminSession{
		Client:    garminHTTPClient(),
		Host:      p.Host,
		ExtID:     p.ExtID,
		Guid:      guid,
		MessageId: messageId,
		ReplyURL:  p.FinalURL,
	}, nil
}

// InitGarminSession follows an inreachlink.com shortlink and builds a reply
// session in one step. The handler fetches the page itself (to also read the
// sender location) and calls newGarminSessionFromPage; this wrapper is kept for
// callers/tests that only need a session from a link.
func InitGarminSession(inreachURL string) (*GarminSession, error) {
	p, err := fetchInReachPage(inreachURL)
	if err != nil {
		return nil, err
	}
	return newGarminSessionFromPage(p)
}

// InitGarminSessionFromState rebuilds a reply session from the tokens persisted
// in Turso (host/extId/guid/messageId), used by the routine 07:00/19:00
// broadcasts where no fresh inreachlink.com shortlink is available. The reply
// POST is authorised by the Guid/MessageId pair and needs no cookies, so this
// requires no network round-trip.
func InitGarminSessionFromState(s SessionState) (*GarminSession, error) {
	if s.Host == "" || s.GUID == "" || s.MessageId == "" {
		return nil, fmt.Errorf("cannot init Garmin session: missing host/guid/messageId in state (hostSet=%v guidSet=%v msgIdSet=%v)",
			s.Host != "", s.GUID != "", s.MessageId != "")
	}
	return &GarminSession{
		Client:    garminHTTPClient(),
		Host:      s.Host,
		ExtID:     s.ExtID,
		Guid:      s.GUID,
		MessageId: s.MessageId,
		ReplyURL:  fmt.Sprintf("https://%s/textmessage/txtmsg?extId=%s", s.Host, url.QueryEscape(s.ExtID)),
	}, nil
}

// ==========================================
// SHORTLINK LOCATION FALLBACK
// ==========================================
//
// Messages composed in the Garmin Messenger / Explore app (relayed over
// Bluetooth) often omit the "Lat .. Lon .." stamp from the email body — only
// messages sent directly from the inReach with a GPS fix include it. The
// sender's location is, however, embedded in the message page behind the
// inreachlink.com shortlink as JSON:
//
//	"Locations":[{ ... "Latitude":<lat>,"Longitude":<lon> ... }]
//
// When the email body has no coordinates we follow the shortlink and recover
// them here. Messages sent with no GPS fix report 0,0 and yield ok=false. The
// implementation lives in internal/garmin (shared with the parse-test endpoint).
func parseShortlinkCoords(pageHTML string) (lat, lon float64, ok bool) {
	return garmin.ParseShortlinkCoords(pageHTML)
}

// garminReplyBody is the JSON payload posted to /TextMessage/TxtMsg, captured
// verbatim from a real browser "Send".
type garminReplyBody struct {
	ReplyAddress string `json:"ReplyAddress"`
	ReplyMessage string `json:"ReplyMessage"`
	Guid         string `json:"Guid"`
	MessageId    string `json:"MessageId"`
}

// SendGarminReply posts the reply (chunked to <160 chars) back to the device via
// the regional explore.garmin.com JSON API:
//
//	POST https://<host>/TextMessage/TxtMsg
//	Content-Type: application/json   X-Requested-With: XMLHttpRequest
//	{"ReplyAddress","ReplyMessage","Guid","MessageId"}  ->  {"Success":true}
//
// The Guid/MessageId pair authorises the POST; no cookies/CSRF token are needed.
func SendGarminReply(session *GarminSession, message string) error {
	if session == nil {
		return fmt.Errorf("no active Garmin session")
	}
	if session.Host == "" || session.Guid == "" || session.MessageId == "" {
		return fmt.Errorf("incomplete Garmin session (hostSet=%v guidSet=%v msgIdSet=%v)",
			session.Host != "", session.Guid != "", session.MessageId != "")
	}

	replyAddr := os.Getenv("EMAIL_USER") // the address the device replies to (our bot inbox)
	postURL := fmt.Sprintf("https://%s/TextMessage/TxtMsg", session.Host)
	origin := fmt.Sprintf("https://%s", session.Host)
	referer := session.ReplyURL
	if referer == "" {
		referer = fmt.Sprintf("https://%s/textmessage/txtmsg?extId=%s", session.Host, session.ExtID)
	}

	chunks := splitForGarmin(message)
	log.Printf("📤 Sending to Garmin: %d chars in %d part(s) [host=%s extId=%s msgId=%s]",
		len(message), len(chunks), session.Host, session.ExtID, session.MessageId)

	for partNum, chunk := range chunks {
		payload, err := json.Marshal(garminReplyBody{
			ReplyAddress: replyAddr,
			ReplyMessage: chunk,
			Guid:         session.Guid,
			MessageId:    session.MessageId,
		})
		if err != nil {
			return fmt.Errorf("failed to marshal reply part %d: %v", partNum+1, err)
		}

		postReq, err := http.NewRequest("POST", postURL, bytes.NewReader(payload))
		if err != nil {
			log.Printf("❌ Failed to build POST request for part %d: %v", partNum+1, err)
			continue
		}
		postReq.Header.Set("Content-Type", "application/json")
		postReq.Header.Set("X-Requested-With", "XMLHttpRequest")
		postReq.Header.Set("Accept", "*/*")
		postReq.Header.Set("User-Agent", browserUA)
		postReq.Header.Set("Origin", origin)
		postReq.Header.Set("Referer", referer)

		resp, err := session.Client.Do(postReq)
		if err != nil {
			return fmt.Errorf("failed to execute POST for part %d: %v", partNum+1, err)
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		log.Printf("📨 Garmin POST part %d/%d → HTTP %d (%d bytes resp)", partNum+1, len(chunks), resp.StatusCode, len(respBody))

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("garmin server rejected part %d. HTTP Status: %d, body: %s", partNum+1, resp.StatusCode, snippet(respBody))
		}
		// A 200 is NOT proof of delivery — Garmin returns {"Success":true} on a
		// genuine send and an error page / {"Success":false} otherwise.
		if !garminSendOK(respBody) {
			return fmt.Errorf("garmin did not confirm delivery of part %d (no \"Success\":true). body: %s", partNum+1, snippet(respBody))
		}
		log.Printf("✅ Sent to Garmin: part %d/%d (%d chars)", partNum+1, len(chunks), len(chunk))

		// Small delay so multi-part messages aren't rate-limited and arrive in order.
		if partNum < len(chunks)-1 {
			time.Sleep(1500 * time.Millisecond)
		}
	}

	return nil
}

// garminSendOK reports whether the TxtMsg JSON response confirmed delivery
// ({"Success":true}). Anything else (false, an error/HTML page) is a failure.
func garminSendOK(b []byte) bool {
	var r struct {
		Success bool `json:"Success"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return false
	}
	return r.Success
}

// snippet returns a single-line, length-capped preview of a response body for logs.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// looksLikeGarminError heuristically detects a Garmin error/anti-bot page that
// was returned with a 2xx/3xx status instead of a real rejection code.
func looksLikeGarminError(b []byte) bool {
	l := strings.ToLower(string(b))
	for _, marker := range []string{
		"requestverificationtoken", // the form was re-served → our POST wasn't accepted
		"captcha",
		"access denied",
		"request blocked",
		"an error occurred",
		"<title>error",
	} {
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
}

func splitForGarmin(msg string) []string {
	if len(msg) <= 160 {
		return []string{msg}
	}
	if i := strings.Index(msg, " | D2 "); i != -1 {
		return []string{msg[:i], msg[i+3:]}
	}
	cut := 160
	for cut > 0 && msg[cut] != ' ' {
		cut--
	}
	if cut == 0 {
		cut = 160
	}
	return []string{msg[:cut], strings.TrimSpace(msg[cut:])}
}

// ==========================================
// EMAIL (MIME DECODING + SMTP REPLY)
// ==========================================

// extractEmailBody walks the raw RFC 822 message and returns the decoded,
// human-readable body text.
//
// msg.GetBody(&imap.BodySectionName{}) hands back the *raw* message: MIME
// headers, multipart boundaries, and content that is usually quoted-printable
// or base64 encoded. Regexing that directly fails silently on anything that
// isn't simple 7-bit plain text (Garmin/Gmail multipart messages, "=2E"
// soft line breaks splitting coordinates, etc.).
//
// go-message/mail transparently decodes Content-Transfer-Encoding and
// charsets per part. We prefer the first text/plain part; if only HTML
// exists, we strip it to text via goquery. As a last resort we return the
// raw string so legacy behaviour is preserved.
func extractEmailBody(raw []byte) string {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		// Not parseable as a MIME message — fall back to the raw bytes.
		log.Printf("MIME: CreateReader failed (%v), falling back to raw body", err)
		return string(raw)
	}

	var plain, html string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("MIME: NextPart error: %v", err)
			break
		}

		h, ok := p.Header.(*mail.InlineHeader)
		if !ok {
			continue // skip attachments
		}
		ctype, _, _ := h.ContentType()
		b, rerr := io.ReadAll(p.Body)
		if rerr != nil {
			continue
		}
		switch ctype {
		case "text/plain":
			if plain == "" {
				plain = string(b)
			}
		case "text/html":
			if html == "" {
				html = string(b)
			}
		}
	}

	if plain != "" {
		return plain
	}
	if html != "" {
		if doc, err := goquery.NewDocumentFromReader(strings.NewReader(html)); err == nil {
			return doc.Text()
		}
		return html
	}
	return string(raw)
}

// sanitizeHeaderValue strips CR/LF so attacker-controlled values (inbound
// Subject, Reply-To) can't inject extra SMTP headers into our reply.
func sanitizeHeaderValue(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func sendTestEmailReply(toEmail, report, inReplyToMsgID, origSubject string) error {
	from := os.Getenv("EMAIL_USER")
	pass := os.Getenv("EMAIL_PASS")
	host := "smtp.gmail.com"
	port := "587"

	if from == "" || pass == "" {
		err := fmt.Errorf("EMAIL_USER or EMAIL_PASS not set")
		log.Printf("❌ Cannot send test email: %v\n", err)
		return err
	}

	toEmail = sanitizeHeaderValue(toEmail)
	subject := sanitizeHeaderValue(origSubject)
	if subject == "" {
		subject = "Alpine Weather Test Report"
	} else if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}

	msgID := fmt.Sprintf("<%d.%d@%s>", time.Now().UnixNano(), os.Getpid(), host)

	headers := []string{
		"From: " + from,
		"To: " + toEmail,
		"Subject: " + subject,
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"Message-ID: " + msgID,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
	}
	if inReplyToMsgID != "" {
		inReplyToMsgID = sanitizeHeaderValue(inReplyToMsgID)
		headers = append(headers, "In-Reply-To: "+inReplyToMsgID)
		headers = append(headers, "References: "+inReplyToMsgID)
	}

	body := "Alpine Weather Report (short condensed for Garmin inreach/messenger):\r\n\r\n" + report + "\r\n"
	msg := []byte(strings.Join(headers, "\r\n") + "\r\n\r\n" + body)

	auth := smtp.PlainAuth("", from, pass, host)
	err := smtp.SendMail(host+":"+port, auth, from, []string{toEmail}, msg)
	if err != nil {
		log.Printf("❌ Failed to send test email to %s: %v\n", toEmail, err)
		return err
	}
	log.Printf("✅ Test email successfully sent to %s\n", toEmail)
	return nil
}

// ==========================================
// DEBUG LOG CAPTURE
// ==========================================
//
// The bot runs as a short-lived serverless function (Netlify cron / Lambda),
// so a webpage cannot tail its stdout. logCapture tees every log line to the
// original writer (stdout → Netlify/CloudWatch) AND buffers it, so the whole
// invocation's logs can be flushed into the debug_log table in one batch and
// streamed to the live log viewer at /debug.html.
type logLine struct {
	t    int64
	text string
}

type logCapture struct {
	out   io.Writer
	runID string
	mu    sync.Mutex
	lines []logLine
}

// logPrefixRe strips Go's default "2009/01/23 01:23:23 " log prefix from the
// stored copy (we keep our own ts column); the stdout copy is untouched.
var logPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?\s+`)

func (l *logCapture) Write(p []byte) (int, error) {
	n, err := l.out.Write(p)
	text := logPrefixRe.ReplaceAllString(strings.TrimRight(string(p), "\r\n"), "")
	if text != "" {
		l.mu.Lock()
		l.lines = append(l.lines, logLine{t: time.Now().Unix(), text: text})
		l.mu.Unlock()
	}
	return n, err
}

// flush persists the buffered lines to debug_log and prunes rows older than two
// days. It must not call into the standard logger (that would recurse).
func (l *logCapture) flush(db *sql.DB) {
	l.mu.Lock()
	lines := l.lines
	l.lines = nil
	l.mu.Unlock()
	if db == nil || len(lines) == 0 {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		return
	}
	stmt, err := tx.Prepare(`INSERT INTO debug_log (ts, seq, run, msg) VALUES (?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return
	}
	for i, ln := range lines {
		if _, err := stmt.Exec(ln.t, i, l.runID, ln.text); err != nil {
			break
		}
	}
	_ = stmt.Close()
	_ = tx.Commit()

	_, _ = db.Exec(`DELETE FROM debug_log WHERE ts < ?`, time.Now().Unix()-2*24*3600)
}

// newRunID returns a short, time-ordered id used to group all log lines emitted
// by a single handler invocation in the viewer.
func newRunID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// ==========================================
// DATABASE (TURSO)
// ==========================================

func connectTurso() (*sql.DB, error) {
	dbURL := os.Getenv("TURSO_DB_URL")
	token := os.Getenv("TURSO_AUTH_TOKEN")
	return sql.Open("libsql", fmt.Sprintf("%s?authToken=%s", dbURL, token))
}

func ensureSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS session_state (
			id              TEXT PRIMARY KEY,
			ext_id          TEXT    NOT NULL DEFAULT '',
			guid            TEXT    NOT NULL DEFAULT '',
			active          INTEGER NOT NULL DEFAULT 0,
			lat             REAL    NOT NULL DEFAULT 0,
			lon             REAL    NOT NULL DEFAULT 0,
			alt             INTEGER NOT NULL DEFAULT 2000,
			park            TEXT    NOT NULL DEFAULT 'arthurs-pass',
			last_fetch      INTEGER NOT NULL DEFAULT 0,
			last_routine_nz TEXT    NOT NULL DEFAULT ''
		)`)
	if err != nil {
		return fmt.Errorf("create table: %w", err)
	}

	// Idempotent column additions for DBs created before these fields existed.
	for _, col := range []string{
		`ALTER TABLE session_state ADD COLUMN last_routine_nz TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE session_state ADD COLUMN message_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE session_state ADD COLUMN reply_host TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE session_state ADD COLUMN paused_until INTEGER NOT NULL DEFAULT 0`,
	} {
		_, err = db.Exec(col)
		if err != nil && !strings.Contains(err.Error(), "duplicate column") && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("alter table: %w", err)
		}
	}

	_, err = db.Exec(`
		INSERT INTO session_state (id) VALUES ('garmin_primary')
		ON CONFLICT(id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("seed row: %w", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS request_log (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			ts      INTEGER NOT NULL,
			source  TEXT    NOT NULL,
			client  TEXT    NOT NULL,
			command TEXT    NOT NULL,
			lat     REAL    NOT NULL DEFAULT 0,
			lon     REAL    NOT NULL DEFAULT 0,
			park    TEXT    NOT NULL DEFAULT ''
		)`)
	if err != nil {
		return fmt.Errorf("create request_log: %w", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS debug_log (
			id  INTEGER PRIMARY KEY AUTOINCREMENT,
			ts  INTEGER NOT NULL,
			seq INTEGER NOT NULL,
			run TEXT    NOT NULL,
			msg TEXT    NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("create debug_log: %w", err)
	}
	return nil
}

func logRequest(db *sql.DB, source, client, command string, lat, lon float64, park string) {
	_, err := db.Exec(
		`INSERT INTO request_log (ts, source, client, command, lat, lon, park) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), source, client, command, lat, lon, park,
	)
	if err != nil {
		log.Printf("logRequest: %v", err)
	}
}

func loadState(db *sql.DB) (SessionState, error) {
	var s SessionState
	var activeInt int
	err := db.QueryRow(
		`SELECT ext_id, guid, active, lat, lon, alt, park, last_fetch, IFNULL(last_routine_nz,''), IFNULL(message_id,''), IFNULL(reply_host,'') FROM session_state WHERE id='garmin_primary'`,
	).Scan(&s.ExtID, &s.GUID, &activeInt, &s.Lat, &s.Lon, &s.Alt, &s.Park, &s.LastFetch, &s.LastRoutineNZ, &s.MessageId, &s.Host)
	if err != nil && (strings.Contains(err.Error(), "last_routine") || strings.Contains(err.Error(), "message_id") || strings.Contains(err.Error(), "reply_host")) {
		err = db.QueryRow(
			`SELECT ext_id, guid, active, lat, lon, alt, park, last_fetch FROM session_state WHERE id='garmin_primary'`,
		).Scan(&s.ExtID, &s.GUID, &activeInt, &s.Lat, &s.Lon, &s.Alt, &s.Park, &s.LastFetch)
		s.LastRoutineNZ = ""
		s.MessageId = ""
		s.Host = ""
	}
	s.Active = activeInt == 1
	return s, err
}

// pausedUntil returns the unix epoch (seconds) until which inbound processing is
// suspended (0 = active). Set via the weather-api /pause endpoint; the bot checks
// it at the top of every cycle so a fresh real inReach message can be captured for
// testing without the cron consuming (and burning) its single-use shortlink.
func pausedUntil(db *sql.DB) (int64, error) {
	var until int64
	err := db.QueryRow(
		`SELECT IFNULL(paused_until, 0) FROM session_state WHERE id='garmin_primary'`,
	).Scan(&until)
	if err != nil && strings.Contains(err.Error(), "paused_until") {
		// Column not present yet on an old DB — treat as not paused.
		return 0, nil
	}
	return until, err
}

func saveState(db *sql.DB, s SessionState) error {
	activeInt := 0
	if s.Active {
		activeInt = 1
	}
	_, err := db.Exec(
		`INSERT INTO session_state (id, ext_id, guid, active, lat, lon, alt, park, last_fetch, last_routine_nz, message_id, reply_host)
		 VALUES ('garmin_primary', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   ext_id=excluded.ext_id, guid=excluded.guid, active=excluded.active,
		   lat=excluded.lat, lon=excluded.lon, alt=excluded.alt, park=excluded.park,
		   last_fetch=excluded.last_fetch, last_routine_nz=excluded.last_routine_nz,
		   message_id=excluded.message_id, reply_host=excluded.reply_host`,
		s.ExtID, s.GUID, activeInt, s.Lat, s.Lon, s.Alt, s.Park, s.LastFetch, s.LastRoutineNZ, s.MessageId, s.Host,
	)
	if err != nil && (strings.Contains(err.Error(), "last_routine") || strings.Contains(err.Error(), "message_id") || strings.Contains(err.Error(), "reply_host")) {
		_, err = db.Exec(
			`INSERT INTO session_state (id, ext_id, guid, active, lat, lon, alt, park, last_fetch)
			 VALUES ('garmin_primary', ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			   ext_id=excluded.ext_id, guid=excluded.guid, active=excluded.active,
			   lat=excluded.lat, lon=excluded.lon, alt=excluded.alt, park=excluded.park,
			   last_fetch=excluded.last_fetch`,
			s.ExtID, s.GUID, activeInt, s.Lat, s.Lon, s.Alt, s.Park, s.LastFetch,
		)
	}
	return err
}

// humanAge renders the time since a unix timestamp for trace logs ("never" when
// the timestamp is zero/unset).
func humanAge(unix int64) string {
	if unix == 0 {
		return "never"
	}
	return time.Since(time.Unix(unix, 0)).Truncate(time.Second).String()
}

func routineBroadcastSlot(nowNZ time.Time) string {
	return fmt.Sprintf("%s-%02d", nowNZ.Format("20060102"), nowNZ.Hour())
}

func shouldRoutineBroadcast(state SessionState, nowNZ time.Time) bool {
	if !state.Active || state.ExtID == "" || state.GUID == "" {
		return false
	}
	if state.Lat == 0 && state.Lon == 0 {
		return false
	}
	h := nowNZ.Hour()
	if h != 7 && h != 19 {
		return false
	}
	if nowNZ.Minute() > 4 {
		return false
	}
	if state.LastRoutineNZ == routineBroadcastSlot(nowNZ) {
		return false
	}
	return true
}

// ==========================================
// COMMAND / BODY PARSING (pure — unit-tested)
// ==========================================

// isGarminSender reports whether an email came from the Garmin/inReach gateway
// (vs. a human emailing the bot directly to test).
func isGarminSender(senderEmail string) bool {
	s := strings.ToLower(senderEmail)
	return strings.Contains(s, "garmin.com") || strings.Contains(s, "inreach")
}

// testCommand is the result of parsing a human (non-Garmin) test email.
type testCommand struct {
	IsAll     bool
	IsUpdate  bool
	HasCoords bool
	Lat, Lon  float64
}

var (
	testCoordRe  = regexp.MustCompile(`(?i)update\s+lat:\s*([-\d.]+),\s*long:\s*([-\d.]+)`)
	bareUpdateRe = regexp.MustCompile(`(?im)^\s*update\s*$`)
	allRe        = regexp.MustCompile(`(?im)^\s*all\s*$`)
)

func parseTestCommand(subject, body string) testCommand {
	combined := subject + "\n" + body
	var tc testCommand
	if allRe.MatchString(combined) {
		tc.IsAll = true
		return tc
	}
	if m := testCoordRe.FindStringSubmatch(combined); len(m) == 3 {
		tc.IsUpdate = true
		lat, errA := strconv.ParseFloat(m[1], 64)
		lon, errB := strconv.ParseFloat(m[2], 64)
		if errA == nil && errB == nil {
			tc.HasCoords = true
			tc.Lat, tc.Lon = lat, lon
		}
		return tc
	}
	if bareUpdateRe.MatchString(combined) {
		tc.IsUpdate = true
	}
	return tc
}

// garminCommand is the result of parsing a Garmin/inReach gateway email.
type garminCommand struct {
	Shortlink           string
	HasCoords           bool
	Lat, Lon            float64
	Start, Stop, Update bool
}

var (
	shortlinkRe = regexp.MustCompile(`https://inreachlink\.com/[A-Za-z0-9_]+`)
	// Tolerant of "Lat -44.5 Lon 169.6", "Lat:-44.5 Lon:169.6", "Lat=-44.5, Lon=169.6".
	garminCoordRe = regexp.MustCompile(`(?i)lat[:=\s]+([-\d.]+)[,\s]+lon[:=\s]+([-\d.]+)`)
)

func parseGarminBody(body string) garminCommand {
	var gc garminCommand
	gc.Shortlink = shortlinkRe.FindString(body)
	// Coordinates live in Garmin's boilerplate ("…sent this message from: Lat …
	// Lon …"), so scan the whole body for them.
	if m := garminCoordRe.FindStringSubmatch(body); len(m) == 3 {
		lat, errA := strconv.ParseFloat(m[1], 64)
		lon, errB := strconv.ParseFloat(m[2], 64)
		if errA == nil && errB == nil {
			gc.HasCoords = true
			gc.Lat, gc.Lon = lat, lon
		}
	}
	// Detect commands ONLY in the user's typed portion (before Garmin's fixed
	// boilerplate), so a device name or boilerplate wording can't trip a false
	// START/STOP/UPDATE.
	cmdText := strings.ToUpper(userMessage(body))
	gc.Start = strings.Contains(cmdText, "START")
	gc.Stop = strings.Contains(cmdText, "STOP")
	gc.Update = strings.Contains(cmdText, "UPDATE")
	return gc
}

// userMessage returns the leading portion of a Garmin email body — the text the
// sender actually typed on the device — by cutting at the first line of Garmin's
// fixed boilerplate. If no boilerplate marker is present, the whole body is
// returned (e.g. locally-crafted test emails).
func userMessage(body string) string {
	end := len(body)
	for _, marker := range []string{
		"View the location",
		"View the map",
		"sent this message from",
		"Do not reply directly",
		"This message was sent to you using",
	} {
		if i := strings.Index(body, marker); i >= 0 && i < end {
			end = i
		}
	}
	return body[:end]
}

// ==========================================
// GARMIN DELIVERY CHOKE POINT
// ==========================================

// sendToGarmin is the single path through which every report reaches a device.
// In normal operation it POSTs via SendGarminReply. When GARMIN_DRY_RUN=1 it
// skips Garmin entirely and emails the exact payload to GARMIN_DRY_RUN_REPLY_TO
// (falling back to EMAIL_USER) so the full receive→parse→build→send pipeline can
// be exercised and inspected without a real inReach device or satellite credits.
func sendToGarmin(session *GarminSession, message, label string) error {
	if os.Getenv("GARMIN_DRY_RUN") == "1" {
		to := os.Getenv("GARMIN_DRY_RUN_REPLY_TO")
		if to == "" {
			to = os.Getenv("EMAIL_USER")
		}
		log.Printf("🧪 DRY RUN [%s]: skipping Garmin POST (%d chars); emailing payload to %s", label, len(message), to)
		log.Printf("🧪 DRY RUN [%s] payload: %s", label, message)
		return sendTestEmailReply(to, message, "", "Garmin Dry Run — "+label)
	}
	if session == nil {
		return fmt.Errorf("no active Garmin session")
	}
	return SendGarminReply(session, message)
}

// ==========================================
// MAIN HANDLER
// ==========================================
func handler(ctx context.Context) error {
	// Capture all log output for this invocation so it can be streamed to the
	// /debug.html live viewer. stdout/Netlify logs are preserved.
	logCap := &logCapture{out: log.Writer(), runID: newRunID()}
	log.SetOutput(logCap)

	db, err := connectTurso()
	if err != nil {
		log.Fatalf("Turso connection failed: %v", err)
	}
	defer db.Close()

	if err := ensureSchema(db); err != nil {
		log.Fatalf("DB schema init failed: %v", err)
	}
	// Flush captured logs after the run (registered after db.Close so it runs
	// first, while the connection is still open).
	defer logCap.flush(db)

	// Suspend switch: when /pause has set a future paused_until, skip the whole
	// cycle (IMAP poll AND routine broadcasts) so a fresh real inReach message is
	// left UNSEEN and its single-use shortlink isn't burned. Auto-resumes once the
	// timestamp lapses.
	if until, perr := pausedUntil(db); perr != nil {
		log.Printf("⚠️ Could not read pause flag (continuing): %v", perr)
	} else if now := time.Now().Unix(); now < until {
		nzt := "unknown"
		if loc, lerr := time.LoadLocation("Pacific/Auckland"); lerr == nil {
			nzt = time.Unix(until, 0).In(loc).Format("2006-01-02 15:04 MST")
		}
		log.Printf("⏸️ Processing suspended until %s — skipping poll", nzt)
		return nil
	}

	state, err := loadState(db)
	if err != nil {
		log.Printf("Warning: Failed to load state: %v\n", err)
	}
	log.Printf("📋 State loaded: active=%v extId_set=%v guid_set=%v lat=%.5f lon=%.5f park=%s lastFetch=%s ago dryRun=%v",
		state.Active, state.ExtID != "", state.GUID != "", state.Lat, state.Lon, state.Park,
		humanAge(state.LastFetch), os.Getenv("GARMIN_DRY_RUN") == "1")

	log.Println("Polling IMAP for commands...")
	emailUser := os.Getenv("EMAIL_USER")
	emailPass := os.Getenv("EMAIL_PASS")

	c, err := client.DialTLS("imap.gmail.com:993", nil)
	if err != nil {
		log.Printf("IMAP Connection error: %v", err)
		return nil // Graceful exit on network issue
	}
	defer c.Logout()

	if err := c.Login(emailUser, emailPass); err != nil {
		log.Printf("IMAP Login error. Check App Password: %v", err)
		return nil
	}

	mbox, err := c.Select("INBOX", false)
	if err != nil {
		log.Printf("IMAP Select INBOX error: %v", err)
		return nil
	}

	if mbox.Messages > 0 {
		criteria := imap.NewSearchCriteria()
		criteria.WithoutFlags = []string{imap.SeenFlag}
		seqNums, err := c.Search(criteria)

		if err != nil {
			log.Printf("IMAP Search criteria error: %v", err)
		} else if len(seqNums) > 0 {
			log.Printf("IMAP: Found %d UNSEEN messages.", len(seqNums))
			seqset := new(imap.SeqSet)
			seqset.AddNum(seqNums...)

			section := &imap.BodySectionName{}
			items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}

			// Drain Fetch fully before issuing any other IMAP command. Fetch holds
			// the connection's command lock; calling Store while still iterating
			// the channel deadlocks once the buffer fills.
			messages := make(chan *imap.Message, len(seqNums))
			fetchDone := make(chan error, 1)
			go func() {
				fetchDone <- c.Fetch(seqset, items, messages)
			}()

			var collected []*imap.Message
			for msg := range messages {
				collected = append(collected, msg)
			}
			if ferr := <-fetchDone; ferr != nil {
				log.Printf("IMAP Fetch error: %v", ferr)
			}

			markSeen := func(seqNum uint32) {
				singleSet := new(imap.SeqSet)
				singleSet.AddNum(seqNum)
				item := imap.FormatFlagsOp(imap.AddFlags, true)
				flags := []interface{}{imap.SeenFlag}
				if err := c.Store(singleSet, item, flags, nil); err != nil {
					log.Printf("Failed to mark message %d seen: %v", seqNum, err)
				}
			}

			for _, msg := range collected {
				subject := msg.Envelope.Subject
				var senderEmail string
				if len(msg.Envelope.From) > 0 {
					senderEmail = msg.Envelope.From[0].MailboxName + "@" + msg.Envelope.From[0].HostName
				}
				replyTo := senderEmail
				if len(msg.Envelope.ReplyTo) > 0 {
					replyTo = msg.Envelope.ReplyTo[0].MailboxName + "@" + msg.Envelope.ReplyTo[0].HostName
				}
				log.Printf("Processing email from: %s, Subject: %s", senderEmail, subject)

				// Read the raw body once, then decode the MIME structure into
				// plain text — both the test and Garmin parsers regex this.
				var bodyStr string
				if r := msg.GetBody(section); r != nil {
					raw, _ := io.ReadAll(r)
					bodyStr = extractEmailBody(raw)
				}
				log.Printf("Reading email body (decoded length: %d bytes)", len(bodyStr))

				// --- Identify if the sender is Garmin ---
				isGarminEmail := isGarminSender(senderEmail)
				log.Printf("🧭 Routing: sender=%q isGarmin=%v subject=%q bodyLen=%d", senderEmail, isGarminEmail, subject, len(bodyStr))

				// 1. TEST COMMAND PARSER (subject OR body)
				// Only run this block if the email is NOT from Garmin
				if !isGarminEmail {
					tc := parseTestCommand(subject, bodyStr)
					log.Printf("🧭 Test parse: isAll=%v isUpdate=%v hasCoords=%v lat=%.5f lon=%.5f",
						tc.IsAll, tc.IsUpdate, tc.HasCoords, tc.Lat, tc.Lon)

					// "all" — return forecasts for every registered park
					if tc.IsAll {
						log.Println("🗺️ ALL parks command detected! Fetching forecasts for all parks...")
						logRequest(db, "email", replyTo, "ALL", 0, 0, "all")
						finalMsg := forecast.BuildAllReports()
						sendOK := true
						if err := sendTestEmailReply(replyTo, finalMsg, msg.Envelope.MessageId, subject); err != nil {
							sendOK = false
						}
						if sendOK {
							markSeen(msg.SeqNum)
						} else {
							log.Printf("Leaving message %d unread for retry after SMTP failure.", msg.SeqNum)
						}
						continue
					}

					if tc.IsUpdate {
						log.Println("🧪 Test UPDATE command detected! Fetching immediate weather…")
						testLat, testLon := tc.Lat, tc.Lon
						if !tc.HasCoords {
							testLat, testLon = state.Lat, state.Lon
							log.Printf("🧪 No coords in email — using last-known state coords (%.5f, %.5f)", testLat, testLon)
						}
						sendOK := true
						if testLat == 0 && testLon == 0 {
							log.Println("⚠️ Cannot process test update: No coordinates available (none in email and none stored).")
						} else {
							testPark := forecast.GetClosestPark(testLat, testLon)
							testAlt := forecast.GetElevation(testLat, testLon)
							logRequest(db, "email", replyTo, "UPDATE", testLat, testLon, testPark)
							finalMsg := forecast.BuildReport(testLat, testLon, testAlt, testPark)
							log.Printf("🧪 Built report (%d chars) → emailing reply to %s", len(finalMsg), replyTo)
							if err := sendTestEmailReply(replyTo, finalMsg, msg.Envelope.MessageId, subject); err != nil {
								sendOK = false
							}
						}

						if sendOK {
							markSeen(msg.SeqNum)
						} else {
							log.Printf("Leaving message %d unread for retry after SMTP failure.", msg.SeqNum)
						}
						continue
					}
				} // End of !isGarminEmail block

				// 2. GARMIN COMMAND PARSER
				if bodyStr == "" {
					log.Println("Garmin parser: Body section is empty, skipping.")
					markSeen(msg.SeqNum)
					continue
				}

				gc := parseGarminBody(bodyStr)
				log.Printf("🧭 Garmin parse: shortlink=%v hasCoords=%v lat=%.5f lon=%.5f start=%v stop=%v update=%v",
					gc.Shortlink != "", gc.HasCoords, gc.Lat, gc.Lon, gc.Start, gc.Stop, gc.Update)

				garminDirty := false
				locationChanged := false
				var activeSession *GarminSession // Hoist session so it survives the if-block

				// --- Fetch the inReach message page once (a shortlink appears to
				//     be single-use): it carries BOTH the sender's location and the
				//     Guid/MessageId needed to post the reply. ---
				var page *inReachPage
				if gc.Shortlink != "" {
					log.Printf("🔗 Extracted inReach shortlink: %s", gc.Shortlink)
					if p, ferr := fetchInReachPage(gc.Shortlink); ferr != nil {
						log.Printf("🔗 Failed to fetch inReach page: %v", ferr)
					} else {
						page = p
						log.Printf("🔗 inReach page: host=%s extId=%s (%d bytes)", p.Host, p.ExtID, len(p.HTML))
					}
				} else {
					log.Println("🔗 No inreachlink.com shortlink in email.")
				}

				// --- Coordinates: from the email body if present, else recovered
				//     from the page (Messenger/app messages omit the body stamp but
				//     the page carries a Locations[] JSON block). ---
				lat, lon, haveCoords := gc.Lat, gc.Lon, gc.HasCoords
				if !haveCoords && page != nil {
					if la, lo, ok := parseShortlinkCoords(page.HTML); ok {
						lat, lon, haveCoords = la, lo, true
						log.Printf("📍 Recovered coordinates from shortlink page: Lat=%f Lon=%f", lat, lon)
					} else {
						log.Println("📍 Shortlink page had no GPS fix (0,0) — message was sent without location.")
					}
				}
				if haveCoords {
					newPark := forecast.GetClosestPark(lat, lon)
					locationChanged = (newPark != state.Park) || (lat != state.Lat) || (lon != state.Lon)
					state.Lat = lat
					state.Lon = lon
					state.Park = newPark
					state.Alt = forecast.GetElevation(lat, lon)
					log.Printf("📍 Coordinates: Lat=%f Lon=%f Park=%s Alt=%d (changed=%v)",
						state.Lat, state.Lon, state.Park, state.Alt, locationChanged)
				} else {
					log.Println("📍 No coordinates available (body or shortlink) — using last-known location.")
					if gc.Shortlink == "" && !gc.HasCoords {
						logRequest(db, "garmin", "no link", "update no link found in email", 0.0, 0.0, "no park")
					}
				}

				// --- Establish the reply session from the same page. ---
				if page != nil {
					if session, serr := newGarminSessionFromPage(page); serr != nil {
						log.Printf("❌ Failed to init Garmin session: %v", serr)
					} else {
						activeSession = session
						garminDirty = true
						state.ExtID = session.ExtID
						state.GUID = session.Guid
						state.MessageId = session.MessageId
						state.Host = session.Host
						log.Printf("✅ Garmin session established (host=%s extId=%s msgId=%s)",
							session.Host, session.ExtID, session.MessageId)
					}
				}

				if gc.Start {
					state.Active = true
					garminDirty = true
					log.Println("Action: START tracking.")
					logRequest(db, "garmin", state.ExtID, "START", state.Lat, state.Lon, state.Park)
				} else if gc.Stop {
					state.Active = false
					garminDirty = true
					log.Println("Action: STOP tracking.")
					logRequest(db, "garmin", state.ExtID, "STOP", state.Lat, state.Lon, state.Park)
					if activeSession != nil || os.Getenv("GARMIN_DRY_RUN") == "1" {
						_ = sendToGarmin(activeSession, "Server: Updates Paused.", "STOP")
					}
				}

				isUpdateCmd := gc.Update
				if isUpdateCmd {
					log.Println("Action: UPDATE triggered manually via email.")
				}

				isStale := time.Now().Unix()-state.LastFetch > (12 * 3600)
				dryRun := os.Getenv("GARMIN_DRY_RUN") == "1"
				wantFetch := isUpdateCmd || (state.Active && (locationChanged || isStale))
				log.Printf("🧮 Fetch decision: update=%v active=%v locationChanged=%v stale=%v hasSession=%v dryRun=%v → fetch=%v",
					isUpdateCmd, state.Active, locationChanged, isStale, activeSession != nil, dryRun, wantFetch)

				// IMMEDIATE FETCH LOGIC:
				// If they send "UPDATE", we fetch. Otherwise, if active, we fetch on new location or stale data.
				if wantFetch {
					log.Println("🚀 Immediate fetch triggered! (New location, stale data, or UPDATE cmd)")

					if state.Lat == 0 && state.Lon == 0 {
						log.Println("Cannot fetch weather: no coordinates available.")
						logRequest(db, "garmin", state.ExtID, "coord-zero-value", 0.0, 0.0, "none")
					} else {
						cmd := "AUTO"
						if isUpdateCmd {
							cmd = "UPDATE"
						}
						logRequest(db, "garmin", state.ExtID, cmd, state.Lat, state.Lon, state.Park)

						finalMsg := forecast.BuildReport(state.Lat, state.Lon, state.Alt, state.Park)
						log.Printf("🧮 Built report (%d chars): %s", len(finalMsg), finalMsg)

						if activeSession == nil && !dryRun {
							log.Println("⚠️ Cannot send weather: no active Garmin session (no inreachlink shortlink in email, or session init failed).")
						} else {
							if err := sendToGarmin(activeSession, finalMsg, cmd); err != nil {
								log.Printf("❌ Failed to send weather reply: %v", err)
							} else {
								state.LastFetch = time.Now().Unix()
								garminDirty = true
							}
						}
					}
				}

				if garminDirty {
					if err := saveState(db, state); err != nil {
						log.Printf("Failed to save session state to Turso: %v", err)
					}
				}

				// Ensure the email is marked as seen so the loop doesn't re-process it infinitely
				markSeen(msg.SeqNum)
			}
		} else {
			log.Println("IMAP: No UNSEEN messages found.")
		}
	} else {
		log.Println("IMAP: INBOX is empty.")
	}

	// 4. ROUTINE BROADCAST CHECK (07:00 / 19:00 NZ wall time)
	loc, tzErr := time.LoadLocation("Pacific/Auckland")
	if tzErr != nil {
		log.Printf("Failed to load Pacific/Auckland timezone: %v", tzErr)
		log.Println("No scheduled broadcast needed at this time.")
	} else {
		now := time.Now().In(loc)
		if shouldRoutineBroadcast(state, now) {
			log.Println("🌅 Broadcast window active! Fetching routine weather...")
			slot := routineBroadcastSlot(now)

			logRequest(db, "garmin", state.ExtID, "ROUTINE", state.Lat, state.Lon, state.Park)
			finalMsg := forecast.BuildReport(state.Lat, state.Lon, state.Alt, state.Park)

			// Establish a fresh Garmin session from the Turso-stored credentials
			// (skipped in dry-run, where sendToGarmin emails the payload instead).
			var routineSession *GarminSession
			if os.Getenv("GARMIN_DRY_RUN") != "1" {
				routineSession, err = InitGarminSessionFromState(state)
				if err != nil {
					log.Printf("❌ Failed to init routine Garmin session: %v", err)
					routineSession = nil
				}
			}

			if routineSession != nil || os.Getenv("GARMIN_DRY_RUN") == "1" {
				if err := sendToGarmin(routineSession, finalMsg, "ROUTINE"); err != nil {
					log.Printf("❌ Failed to send routine broadcast: %v", err)
				} else {
					state.LastFetch = time.Now().Unix()
					state.LastRoutineNZ = slot
					if err := saveState(db, state); err != nil {
						log.Printf("Failed to save state after broadcast: %v", err)
					}
				}
			}
		} else {
			log.Println("No scheduled broadcast needed at this time.")
		}
	}

	return nil
}

func main() {
	if os.Getenv("LOCAL_WEATHER_BOT") == "1" {
		if err := handler(context.Background()); err != nil {
			log.Fatal(err)
		}
		return
	}
	lambda.Start(handler)
}
