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

func sendToGarmin(msg, extId, guid string) {
	if len(msg) > 160 {
		msg = msg[:160]
	}
	data := url.Values{}
	data.Set("Message", msg)
	data.Set("extId", extId)
	data.Set("guid", guid)

	resp, err := garminHTTPClient.PostForm("https://explore.garmin.com/TextMessage/TxtMsg", data)
	if err != nil {
		log.Printf("❌ Failed to send to Garmin: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		log.Printf("✅ Successfully sent to Garmin (%d chars): %s\n", len(msg), msg)
	} else {
		log.Printf("❌ Failed to send to Garmin. Status: %d\n", resp.StatusCode)
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
	return nil
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
				} else if strings.Contains(upperBody, "STOP") {
					state.Active = false
					sendToGarmin("Server: Updates Paused.", state.ExtID, state.GUID)
					garminDirty = true
					log.Println("Action: STOP tracking.")
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
				}

				isStale := time.Now().Unix()-state.LastFetch > (12 * 3600)

				// IMMEDIATE FETCH LOGIC:
				// If they send "UPDATE", we fetch. Otherwise, if active, we fetch on new location or stale data.
				if isUpdateCmd || (state.Active && (locationChanged || isStale)) {
					log.Println("🚀 Immediate fetch triggered! (New location, stale data, or UPDATE cmd)")

					if state.Lat == 0 && state.Lon == 0 {
						log.Println("Cannot fetch weather: no coordinates available.")
					} else {
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
