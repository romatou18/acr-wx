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

var garminHTTPClient = &http.Client{Timeout: 5 * time.Second}

func postToGarmin(partNum int, msg, extId, guid string) {
	// 1. Create a transient client with a CookieJar for this specific execution
	// It is critical to create a NEW client here rather than using a global one 
	// to avoid session cross-contamination if multiple requests hit your gateway at once.
	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Printf("❌ Failed to create cookie jar: %v\n", err)
		return
	}
	client := &http.Client{Jar: jar}

	// 2. Construct the GET URL to establish the session
	// This mimics a web browser opening the link provided in the email
	pageURL := fmt.Sprintf("https://explore.garmin.com/TextMessage/TxtMsg?extId=%s&guid=%s", extId, guid)
	
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		log.Printf("❌ Failed to create GET request: %v\n", err)
		return
	}
	// Mimic a standard browser to avoid triggering basic bot-defense mechanisms
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("❌ Failed to fetch Garmin page: %v\n", err)
		return
	}
	defer resp.Body.Close()

	// 3. Parse the HTML to extract the CSRF Token
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		log.Printf("❌ Failed to parse HTML: %v\n", err)
		return
	}

	token, exists := doc.Find(`input[name="__RequestVerificationToken"]`).Attr("value")
	if !exists {
		log.Printf("❌ Could not find __RequestVerificationToken. Link may be expired or malformed.\n")
		return
	}

// Extracting coordinates 
// Example: Using regex to find the coordinates in the raw HTML string
htmlContent := doc.Text() 
latLonRegex := regexp.MustCompile(`"Latitude":\s*(-?\d+\.\d+),\s*"Longitude":\s*(-?\d+\.\d+)`)
matches := latLonRegex.FindStringSubmatch(htmlContent)

if len(matches) == 3 {
    latitude := matches[1]
    longitude := matches[2]
    log.Printf("Extracted coordinates: %s, %s\n", latitude, longitude)
    
    // Pass these coordinates to your MetService API integration
} else {
    log.Println("Could not parse coordinates. The device may not have had a GPS lock.")
 return
}

	// 4. Build the POST payload, now including the vital security token
	data := url.Values{}
	data.Set("__RequestVerificationToken", token)
	data.Set("Message", msg)
	data.Set("extId", extId)
	data.Set("guid", guid)

	// 5. Submit the POST request
	postURL := "https://explore.garmin.com/TextMessage/TxtMsg"
	postReq, err := http.NewRequest("POST", postURL, strings.NewReader(data.Encode()))
	if err != nil {
		log.Printf("❌ Failed to create POST request: %v\n", err)
		return
	}
	
	// Add required headers for a form submission
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	// The Referer header is often strictly validated by ASP.NET backend servers
	postReq.Header.Set("Referer", pageURL) 

	// Execute POST using the same client, which automatically attaches the cookies saved from the GET request
	postResp, err := client.Do(postReq)
	if err != nil {
		log.Printf("❌ Failed to send to Garmin (part %d): %v\n", partNum, err)
		return
	}
	defer postResp.Body.Close()

	// 6. Evaluate response
	if postResp.StatusCode == 200 {
		log.Printf("✅ Sent to Garmin part %d (%d chars): %s\n", partNum, len(msg), msg)
	} else {
		log.Printf("❌ Failed to send to Garmin part %d. HTTP Status: %d\n", partNum, postResp.StatusCode)
	}
}

func sendToGarmin(msg, extId, guid string) {
	// Dry-run mode: email the full (unsplit) report instead of POSTing to Garmin.
	// Set GARMIN_DRY_RUN=1 and optionally GARMIN_DRY_RUN_REPLY_TO=<address>.
	if os.Getenv("GARMIN_DRY_RUN") == "1" {
		replyTo := os.Getenv("GARMIN_DRY_RUN_REPLY_TO")
		if replyTo == "" {
			replyTo = os.Getenv("EMAIL_USER")
		}
		log.Printf("🔧 GARMIN_DRY_RUN: routing report to %s (extId=%s)\n", replyTo, extId)
		if err := sendTestEmailReply(replyTo, msg, "", "Garmin Dry Run: Weather Report"); err != nil {
			log.Printf("❌ GARMIN_DRY_RUN email failed: %v\n", err)
		}
		return
	}

	for i, part := range splitForGarmin(msg) {
		postToGarmin(i+1, part, extId, guid)
	}
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

func postToGarmin_old(partNum int, msg, extId, guid string) {
	data := url.Values{}
	data.Set("Message", msg)
	data.Set("extId", extId)
	data.Set("guid", guid)

	resp, err := garminHTTPClient.PostForm("https://explore.garmin.com/TextMessage/TxtMsg", data)
	if err != nil {
		log.Printf("❌ Failed to send to Garmin (part %d): %v\n", partNum, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		log.Printf("✅ Sent to Garmin part %d/%d (%d chars): %s\n", partNum, partNum, len(msg), msg)
	} else {
		log.Printf("❌ Failed to send to Garmin part %d. Status: %d\n", partNum, resp.StatusCode)
	}
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

				// 1. TEST COMMAND PARSER (subject OR body)
				testCoordRegex := regexp.MustCompile(`(?i)update\s+lat:\s*([-\d.]+),\s*long:\s*([-\d.]+)`)
				bareUpdateRegex := regexp.MustCompile(`(?im)^\s*update\s*$`)
				allRegex := regexp.MustCompile(`(?im)^\s*all\s*$`)

				combined := subject + "\n" + bodyStr

				// "all" — return forecasts for every registered park
				if allRegex.MatchString(combined) {
					log.Println("🗺️ ALL parks command detected! Fetching forecasts for all parks...")
					logRequest(db, "email", replyTo, "ALL", 0, 0, "all")
					finalMsg := forecast.BuildAllReports()
					log.Printf("📋 All-parks report (%d chars):\n%s\n", len(finalMsg), finalMsg)
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
						log.Printf("📋 Test weather report (%d chars): %s\n", len(finalMsg), finalMsg)
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

				// 2. GARMIN COMMAND PARSER
				if bodyStr == "" {
					log.Println("Garmin parser: Body section is empty, skipping.")
					markSeen(msg.SeqNum)
					continue
				}

				garminDirty := false

				// Check for Garmin Session Tokens
				sessionMatch := regexp.MustCompile(`extId=([^&]+)&guid=([^&]+)`).FindStringSubmatch(bodyStr)
				if len(sessionMatch) == 3 {
					state.ExtID = sessionMatch[1]
					state.GUID = sessionMatch[2]
					garminDirty = true
					log.Printf("Extracted Session Tokens: extId=%s", state.ExtID)
				} else {
					log.Println("No Garmin extId/guid found in email.")
				}

				upperBody := strings.ToUpper(bodyStr)
				if strings.Contains(upperBody, "START") {
					state.Active = true
					garminDirty = true
					log.Println("Action: START tracking.")
					logRequest(db, "garmin", state.ExtID, "START", state.Lat, state.Lon, state.Park)
				} else if strings.Contains(upperBody, "STOP") {
					state.Active = false
					sendToGarmin("Server: Updates Paused.", state.ExtID, state.GUID)
					garminDirty = true
					log.Println("Action: STOP tracking.")
					logRequest(db, "garmin", state.ExtID, "STOP", state.Lat, state.Lon, state.Park)
				}

				isUpdateCmd := strings.Contains(upperBody, "UPDATE")
				if isUpdateCmd {
					log.Println("Action: UPDATE triggered manually via email.")
				}

				locationChanged := false
				coordMatch := regexp.MustCompile(`Lat:\s*([-\d.]+)\s*Lon:\s*([-\d.]+)`).FindStringSubmatch(bodyStr)
				if len(coordMatch) == 3 {
					newLat, _ := strconv.ParseFloat(coordMatch[1], 64)
					newLon, _ := strconv.ParseFloat(coordMatch[2], 64)

					newPark := forecast.GetClosestPark(newLat, newLon)
					locationChanged = (newPark != state.Park) || (newLat != state.Lat) || (newLon != state.Lon)

					state.Lat = newLat
					state.Lon = newLon
					state.Park = newPark
					state.Alt = forecast.GetElevation(newLat, newLon)
					garminDirty = true
					log.Printf("Parsed Coordinates: Lat=%f, Lon=%f, Park=%s", state.Lat, state.Lon, state.Park)
				} else {
					log.Println("No coordinates found in body. Using existing known coordinates.")
logRequest(db, "garmin", state.ExtID, "No-coord-in-email-body", 0.0, 0.0, "none")
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
						sendToGarmin(finalMsg, state.ExtID, state.GUID)

						state.LastFetch = time.Now().Unix()
						garminDirty = true
					}
				}

				if garminDirty {
					if err := saveState(db, state); err != nil {
						log.Printf("Failed to save session state to Turso: %v", err)
					}
				}

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
			sendToGarmin(finalMsg, state.ExtID, state.GUID)

			state.LastFetch = time.Now().Unix()
			state.LastRoutineNZ = slot
			if err := saveState(db, state); err != nil {
				log.Printf("Failed to save state after broadcast: %v", err)
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
