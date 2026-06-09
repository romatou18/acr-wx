package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata"

	"acr-wx/internal/forecast"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
	"net/http/cookiejar"
	"github.com/PuerkitoBio/goquery"
)


// ==========================================
// CONFIGURATION & STRUCTS
// ==========================================
type SessionState struct {
	ExtID         string
	GUID          string
	Active        bool
	Lat           float64
	Lon           float64
	Alt           int
	Park          string
	LastFetch     int64
	LastRoutineNZ string
}


// GarminSession holds the live connection state between the extraction phase and the posting phase
type GarminSession struct {
	Client *http.Client
	Token  string
	ExtID  string
	Guid   string
}



	// Phase 1: Establish the session from a shortlink and grab the CSRF token
func InitGarminSession(inreachURL string) (*GarminSession, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second, // Protects Netlify from hanging
	}

	req, _ := http.NewRequest("GET", inreachURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Extract ExtID and GUID from the final redirected URL
	finalURL := resp.Request.URL
	extId := finalURL.Query().Get("extId")
	guid := finalURL.Query().Get("guid")

	if extId == "" || guid == "" {
		return nil, fmt.Errorf("redirected URL did not contain extId/guid: %s", finalURL.String())
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}

	// Extract the vital CSRF Token
	token, exists := doc.Find(`input[name="__RequestVerificationToken"]`).Attr("value")
	if !exists {
		return nil, fmt.Errorf("CSRF token not found in the HTML")
	}

	session := &GarminSession{
		Client: client,
		Token:  token,
		ExtID:  extId,
		Guid:   guid,
	}

	return session, nil
}

// Helper for cron broadcasts that don't have an email shortlink
func InitGarminSessionFromState(extId, guid string) (*GarminSession, error) {
	longURL := fmt.Sprintf("https://explore.garmin.com/TextMessage/TxtMsg?extId=%s&guid=%s", extId, guid)
	return InitGarminSession(longURL)
}


// Phase 2: Post the weather report using the EXACT SAME session
func SendGarminReply(session *GarminSession, message string) error {
	// Dry-run mode: email the full report instead of POSTing to Garmin.
	if os.Getenv("GARMIN_DRY_RUN") == "1" {
		replyTo := os.Getenv("GARMIN_DRY_RUN_REPLY_TO")
		if replyTo == "" {
			replyTo = os.Getenv("EMAIL_USER")
		}
		log.Printf("🔧 GARMIN_DRY_RUN: routing report to %s (extId=%s)\n", replyTo, session.ExtID)
		if err := sendTestEmailReply(replyTo, message, "", "Garmin Dry Run: Weather Report"); err != nil {
			log.Printf("❌ GARMIN_DRY_RUN email failed: %v\n", err)
		}
		return nil
	}

	// Pre-load the session constants
	data := url.Values{}
	data.Set("__RequestVerificationToken", session.Token)
	data.Set("extId", session.ExtID)
	data.Set("guid", session.Guid)

	postURL := "https://explore.garmin.com/TextMessage/TxtMsg"
	refererURL := fmt.Sprintf("https://explore.garmin.com/TextMessage/TxtMsg?extId=%s&guid=%s", session.ExtID, session.Guid)

	// Iterate over the chunks and send them one by one
	for partNum, part := range splitForGarmin(message) {
		data.Set("Message", part) // FIX: Using the chunk, not the whole message

		postReq, err := http.NewRequest("POST", postURL, strings.NewReader(data.Encode()))
		if err != nil {
			log.Printf("❌ Failed to build request for part %d: %v\n", partNum+1, err)
			continue
		}

		postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		postReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
		postReq.Header.Set("Referer", refererURL)

		resp, err := session.Client.Do(postReq)
		if err != nil {
			return fmt.Errorf("failed to send part %d: %v", partNum+1, err)
		}

		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("garmin rejected part %d with HTTP %d", partNum+1, resp.StatusCode)
		}
		
		log.Printf("✅ Sent to Garmin part %d/%d (%d chars)", partNum+1, len(splitForGarmin(message)), len(part))
		resp.Body.Close() // FIX: Close manually inside the loop to prevent descriptor leaks
	}
	
	return nil
}

// splitForGarmin splits a report into ≤160-char chunks for the Garmin inReach
// 160-char SMS limit. The natural boundary is " | D2 " which separates the
// Day 1 and Day 2 forecast sections.
func splitForGarmin(msg string) []string {
	if len(msg) <= 160 {
		return []string{msg}
	}
	if i := strings.Index(msg, " | D2 "); i != -1 {
		return []string{msg[:i], msg[i+3:]}
	}
	// Fallback: find the last space before byte 160 and split there.
	cut := 160
	for cut > 0 && msg[cut] != ' ' {
		cut--
	}
	if cut == 0 {
		cut = 160
	}
	return []string{msg[:cut], strings.TrimSpace(msg[cut:])}
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

	subject := strings.TrimSpace(origSubject)
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

	// Idempotent: add last_routine_nz if this is an older DB that predates it.
	_, err = db.Exec(`ALTER TABLE session_state ADD COLUMN last_routine_nz TEXT NOT NULL DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("alter table: %w", err)
	}

	// Seed the one-and-only control row if it doesn't exist yet.
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
		`SELECT ext_id, guid, active, lat, lon, alt, park, last_fetch, IFNULL(last_routine_nz,'') FROM session_state WHERE id='garmin_primary'`,
	).Scan(&s.ExtID, &s.GUID, &activeInt, &s.Lat, &s.Lon, &s.Alt, &s.Park, &s.LastFetch, &s.LastRoutineNZ)
	if err != nil && strings.Contains(err.Error(), "last_routine") {
		err = db.QueryRow(
			`SELECT ext_id, guid, active, lat, lon, alt, park, last_fetch FROM session_state WHERE id='garmin_primary'`,
		).Scan(&s.ExtID, &s.GUID, &activeInt, &s.Lat, &s.Lon, &s.Alt, &s.Park, &s.LastFetch)
		s.LastRoutineNZ = ""
	}
	s.Active = activeInt == 1
	return s, err
}

func saveState(db *sql.DB, s SessionState) error {
	activeInt := 0
	if s.Active {
		activeInt = 1
	}
	_, err := db.Exec(
		`INSERT INTO session_state (id, ext_id, guid, active, lat, lon, alt, park, last_fetch, last_routine_nz)
		 VALUES ('garmin_primary', ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   ext_id=excluded.ext_id, guid=excluded.guid, active=excluded.active,
		   lat=excluded.lat, lon=excluded.lon, alt=excluded.alt, park=excluded.park,
		   last_fetch=excluded.last_fetch, last_routine_nz=excluded.last_routine_nz`,
		s.ExtID, s.GUID, activeInt, s.Lat, s.Lon, s.Alt, s.Park, s.LastFetch, s.LastRoutineNZ,
	)
	if err != nil && strings.Contains(err.Error(), "last_routine") {
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
// MAIN HANDLER
// ==========================================

func handler(ctx context.Context) error {
	db, err := connectTurso()
	if err != nil {
		log.Fatalf("Turso connection failed: %v", err)
	}
	defer db.Close()

	if err := ensureSchema(db); err != nil {
		log.Fatalf("DB schema init failed: %v", err)
	}

	state, err := loadState(db)
	if err != nil {
		log.Printf("Warning: Failed to load state: %v\n", err)
	}

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

				// Read body once — both test and Garmin parsers need it.
				var bodyStr string
				if r := msg.GetBody(section); r != nil {
					b, _ := io.ReadAll(r)
					bodyStr = string(b)
				}
				log.Printf("Reading email body (Length: %d bytes)", len(bodyStr))

						// ... (Inside the IMAP loop, just below where you read the bodyStr) ...
				log.Printf("Reading email body (Length: %d bytes)", len(bodyStr))

				// --- NEW CHECK: Identify if the sender is Garmin ---
				isGarminEmail := strings.Contains(strings.ToLower(senderEmail), "garmin.com") || strings.Contains(strings.ToLower(senderEmail), "inreach")


				// 1. TEST COMMAND PARSER (subject OR body)
				// Only run this block if the email is NOT from Garmin
				if !isGarminEmail {
					testCoordRegex := regexp.MustCompile(`(?i)update\s+lat:\s*([-\d.]+),\s*long:\s*([-\d.]+)`)
					bareUpdateRegex := regexp.MustCompile(`(?im)^\s*update\s*$`)
					allRegex := regexp.MustCompile(`(?im)^\s*all\s*$`)

					combined := subject + "\n" + bodyStr

					// "all" — return forecasts for every registered park
					if allRegex.MatchString(combined) {
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

					var testLat, testLon float64
					isTest := false

					if match := testCoordRegex.FindStringSubmatch(combined); len(match) == 3 {
						testLat, _ = strconv.ParseFloat(match[1], 64)
						testLon, _ = strconv.ParseFloat(match[2], 64)
						isTest = true
					} else if bareUpdateRegex.MatchString(combined) {
						testLat = state.Lat
						testLon = state.Lon
						isTest = true
					}

					if isTest {
						log.Println("🧪 Test command detected! Fetching immediate weather...")
						sendOK := true
						if testLat == 0 && testLon == 0 {
							log.Println("Cannot process test update: No coordinates available.")
						} else {
							testPark := forecast.GetClosestPark(testLat, testLon)
							testAlt := forecast.GetElevation(testLat, testLon)
							logRequest(db, "email", replyTo, "UPDATE", testLat, testLon, testPark)
							finalMsg := forecast.BuildReport(testLat, testLon, testAlt, testPark)
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

				garminDirty := false
				locationChanged := false
				var activeSession *GarminSession // Hoist session so it survives the if-block
					
				// Check for Garmin inReach Shortlink
				linkRegex := regexp.MustCompile(`https://inreachlink\.com/[A-Za-z0-9]+`)
				shortlink := linkRegex.FindString(bodyStr)
				
				if shortlink != "" {
					log.Printf("Extracted inReach Link: %s", shortlink)
					
					// --- Extract Coordinates from Email Body ---
					// Matches: Lat -45.009731 Lon 168.896792
					latLonRegex := regexp.MustCompile(`Lat\s*([-\d.]+)\s*Lon\s*([-\d.]+)`)
					coordMatch := latLonRegex.FindStringSubmatch(bodyStr)

					if len(coordMatch) == 3 {
						newLat, _ := strconv.ParseFloat(coordMatch[1], 64)
						newLon, _ := strconv.ParseFloat(coordMatch[2], 64)
						
						newPark := forecast.GetClosestPark(newLat, newLon)
						locationChanged = (newPark != state.Park) || (newLat != state.Lat) || (newLon != state.Lon)

						state.Lat = newLat
						state.Lon = newLon
						state.Park = newPark
						state.Alt = forecast.GetElevation(newLat, newLon)
						
						log.Printf("Parsed Coordinates from Email: Lat=%f, Lon=%f, Park=%s", state.Lat, state.Lon, state.Park)
					} else {
						log.Println("No coordinates found in the plain text email body.")
						logRequest(db, "garmin", state.ExtID, "No-coord-in-email", 0.0, 0.0, "none")
					}
					
					// Establish the security session for the reply by following the shortlink
					session, err := InitGarminSession(shortlink)
					if err != nil {
						log.Printf("❌ Failed to init Garmin session: %v", err)
					} else {
						activeSession = session
						garminDirty = true
						
						// Save the ExtID and GUID to Turso for routine cron broadcasts
						state.ExtID = session.ExtID
						state.GUID = session.Guid
					}
				} else {
					log.Println("No inreachlink.com URL found in email.")
					logRequest(db, "garmin", "no link", "update no link found in email", 0.0, 0.0, "no park")
				}

				upperBody := strings.ToUpper(bodyStr)
                // ... (The START / STOP / UPDATE logic continues directly below this) ...

				if strings.Contains(upperBody, "START") {
					state.Active = true
					garminDirty = true
					log.Println("Action: START tracking.")
					logRequest(db, "garmin", state.ExtID, "START", state.Lat, state.Lon, state.Park)
				} else if strings.Contains(upperBody, "STOP") {
					state.Active = false
					garminDirty = true
					log.Println("Action: STOP tracking.")
					logRequest(db, "garmin", state.ExtID, "STOP", state.Lat, state.Lon, state.Park)
					
					if activeSession != nil {
						_ = SendGarminReply(activeSession, "Server: Updates Paused.")
					}
				}

				isUpdateCmd := strings.Contains(upperBody, "UPDATE")
				if isUpdateCmd {
					log.Println("Action: UPDATE triggered manually via email.")
				}

				isStale := time.Now().Unix()-state.LastFetch > (12 * 3600)

				// IMMEDIATE FETCH LOGIC:
				// If they send "UPDATE", we fetch. Otherwise, if active, we fetch on new location or stale data.
				if isUpdateCmd || (state.Active && (locationChanged || isStale)) {
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
						
						if activeSession != nil {
							err := SendGarminReply(activeSession, finalMsg)
							if err != nil {
								log.Printf("❌ Failed to send weather reply: %v", err)
							} else {
								state.LastFetch = time.Now().Unix()
								garminDirty = true
							}
						} else {
							log.Println("⚠️ Cannot send weather: No active Garmin session.")
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
			
			// Establish a fresh Garmin Session using the Turso-stored credentials
			routineSession, _, _, err := InitGarminSessionFromState(state.ExtID, state.GUID)
			if err != nil {
				log.Printf("❌ Failed to init routine Garmin session: %v", err)
			} else {
				err = SendGarminReply(routineSession, finalMsg)
				if err != nil {
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
