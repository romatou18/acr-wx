package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// ==========================================
// CONFIGURATION & STRUCTS
// ==========================================
const UserAgent = "AlpineWeatherBot/1.0 (contact: test@example.com)"

type SessionState struct {
	ExtID  string
	GUID   string
	Active bool
	Lat    float64
	Lon    float64
	Alt    int
	Park   string
}

type ParkInfo struct {
	Lat    float64
	Lon    float64
	NzaaID int
}

var PARKS = map[string]ParkInfo{
	"arthurs-pass":         {Lat: -42.94, Lon: 171.56, NzaaID: 4},
	"craigieburn":          {Lat: -43.13, Lon: 171.71, NzaaID: 5},
	"aoraki-mt-cook":       {Lat: -43.73, Lon: 170.09, NzaaID: 7},
	"westland-tai-poutini": {Lat: -43.41, Lon: 170.18, NzaaID: 7},
	"mt-aspiring":          {Lat: -44.39, Lon: 168.72, NzaaID: 15},
	"nelson-lakes":         {Lat: -41.90, Lon: 172.68, NzaaID: 13},
}

// HTTP Client with 5-second timeout to prevent Lambda hanging
var httpClient = &http.Client{Timeout: 5 * time.Second}

// ==========================================
// FORECAST FETCHERS
// ==========================================

func fetchYrNo(lat, lon float64, alt int) string {
	targetURL := fmt.Sprintf("https://api.met.no/weatherapi/locationforecast/2.0/compact?lat=%f&lon=%f&altitude=%d", lat, lon, alt)
	
	req, _ := http.NewRequest("GET", targetURL, nil)
	req.Header.Set("User-Agent", UserAgent)
	
	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return "YR:Err"
	}
	defer resp.Body.Close()

	// Anonymous struct to parse only the fields we need from the massive JSON
	var yrResp struct {
		Properties struct {
			Timeseries []struct {
				Data struct {
					Instant struct {
						Details struct {
							AirTemp float64 `json:"air_temperature"`
						} `json:"details"`
					} `json:"instant"`
					Next1Hours struct {
						Details struct {
							Precip float64 `json:"precipitation_amount"`
						} `json:"details"`
					} `json:"next_1_hours"`
				} `json:"data"`
			} `json:"timeseries"`
		} `json:"properties"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&yrResp); err != nil {
		return "YR:JSON_Err"
	}

	ts := yrResp.Properties.Timeseries
	if len(ts) == 0 {
		return "YR:NoData"
	}

	temp := int(math.Round(ts[0].Data.Instant.Details.AirTemp))
	
	// Safely calculate Day 1 and Day 2 precip
	var d1Precip, d2Precip float64
	d1Limit := min(24, len(ts))
	d2Limit := min(48, len(ts))

	for i := 0; i < d1Limit; i++ {
		d1Precip += ts[i].Data.Next1Hours.Details.Precip
	}
	for i := d1Limit; i < d2Limit; i++ {
		d2Precip += ts[i].Data.Next1Hours.Details.Precip
	}

	return fmt.Sprintf("YR T:%dC D1:%dmm D2:%dmm", temp, int(math.Round(d1Precip)), int(math.Round(d2Precip)))
}

func fetchMetService(park string) string {
	targetURL := fmt.Sprintf("https://www.metservice.com/mountains-and-parks/national-parks/%s", park)
	
	req, _ := http.NewRequest("GET", targetURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0") // Disguise as browser
	
	resp, err := httpClient.Do(req)
	if err != nil {
		return "MS:Err"
	}
	defer resp.Body.Close()
	
	bodyBytes, _ := io.ReadAll(resp.Body)
	html := string(bodyBytes)

	// RegEx to scrape HTML (Go's regex doesn't support lookarounds, so we capture differently)
	reW1k := regexp.MustCompile(`(?i)Wind at 1000 metres.*?(\d{2,3})\s*km/h`)
	reW2k := regexp.MustCompile(`(?i)Wind at 2000 metres.*?(\d{2,3})\s*km/h`)
	reW3k := regexp.MustCompile(`(?i)Wind at 3000 metres.*?(\d{2,3})\s*km/h`)
	reFcst := regexp.MustCompile(`(?i)Forecast\.\s*(.*?)\s*Wind`)

	w1kMatches := reW1k.FindAllStringSubmatch(html, -1)
	w2kMatches := reW2k.FindAllStringSubmatch(html, -1)
	w3kMatches := reW3k.FindAllStringSubmatch(html, -1)
	fcstMatches := reFcst.FindAllStringSubmatch(html, -1)

	// Helper to safely get regex match
	getMatch := func(matches [][]string, index int) string {
		if len(matches) > index && len(matches[index]) > 1 {
			return matches[index][1]
		}
		return "??"
	}

	d1W1, d1W2, d1W3 := getMatch(w1kMatches, 0), getMatch(w2kMatches, 0), getMatch(w3kMatches, 0)
	d2W1, d2W2, d2W3 := getMatch(w1kMatches, 1), getMatch(w2kMatches, 1), getMatch(w3kMatches, 1)
	
	d1Txt := compressMetServiceText(getMatch(fcstMatches, 0))
	d2Txt := compressMetServiceText(getMatch(fcstMatches, 1))

	shortPark := strings.ToUpper(park)
	if len(shortPark) > 3 {
		shortPark = shortPark[:3]
	}

	return fmt.Sprintf("MS(%s) D1 %s W1k:%s 2k:%s 3k:%s | D2 %s W1k:%s 2k:%s 3k:%s", 
		shortPark, d1Txt, d1W1, d1W2, d1W3, d2Txt, d2W1, d2W2, d2W3)
}

func fetchAvalanche(parkSlug string) string {
	parkInfo, exists := PARKS[parkSlug]
	if !exists {
		return "AVL:??"
	}

	targetURL := fmt.Sprintf("https://www.avalanche.net.nz/region/%d", parkInfo.NzaaID)
	req, _ := http.NewRequest("GET", targetURL, nil)
	req.Header.Set("User-Agent", UserAgent)
	
	resp, err := httpClient.Do(req)
	if err != nil {
		return "AVL:Err"
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	html := string(bodyBytes)

	// Match e.g. "3 Considerable" or "0 No Rating"
	re := regexp.MustCompile(`(?i)(\d)\s(Low|Moderate|Considerable|High|Extreme|No Rating)`)
	match := re.FindStringSubmatch(html)

	if len(match) == 3 {
		level := match[1]
		text := strings.ToUpper(match[2])
		if len(text) > 4 {
			text = text[:4] // E.g., "CONS", "EXTR"
		}
		if level == "0" {
			return "AVL:CLOSED"
		}
		return fmt.Sprintf("AVL:%s-%s", level, text)
	}

	return "AVL:??"
}

func sendToGarmin(msg, extId, guid string) {
	// Strictly enforce 160 character limit
	if len(msg) > 160 {
		msg = msg[:160]
	}

	endpoint := "https://explore.garmin.com/TextMessage/TxtMsg"
	
	data := url.Values{}
	data.Set("Message", msg)
	data.Set("extId", extId)
	data.Set("guid", guid)

	resp, err := httpClient.PostForm(endpoint, data)
	if err != nil {
		fmt.Printf("❌ Failed to send to Garmin: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		fmt.Printf("✅ Successfully sent to Garmin (%d chars): %s\n", len(msg), msg)
	} else {
		fmt.Printf("❌ Failed to send to Garmin. Status: %d\n", resp.StatusCode)
	}
}

// ==========================================
// HELPERS
// ==========================================

func compressMetServiceText(text string) string {
	replacements := map[string]string{
		"Partly cloudy": "PrtlyCldy", "Mostly cloudy": "MstlyCldy",
		"isolated showers": "IsoShwrs", "scattered showers": "SctShwrs",
		"heavy rain": "HvyRain", "falling as snow": "Snow", "showers": "Shwrs",
		"developing": "dev", "morning": "AM", "afternoon": "PM", "evening": "Eve",
		"Fine": "Clear", "turning to": "->", "easing": "eas", "with": "w/",
	}

	for old, newStr := range replacements {
		text = strings.ReplaceAll(text, old, newStr)
		text = strings.ReplaceAll(text, strings.Title(old), newStr) // Handle Capitalization
	}

	// Clean extra spaces and truncate
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 15 {
		text = strings.TrimSpace(text[:15])
	}
	return text
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sendTestEmailReply(toEmail, report string) {
	from := os.Getenv("EMAIL_USER")
	pass := os.Getenv("EMAIL_PASS")
	host := "smtp.gmail.com"
	port := "587"

	msg := []byte("To: " + toEmail + "\r\n" +
		"Subject: Alpine Weather Test Report\r\n" +
		"\r\n" +
		"Here is your simulated Alpine Weather Report:\r\n\n" +
		report + "\r\n")

	auth := smtp.PlainAuth("", from, pass, host)
	err := smtp.SendMail(host+":"+port, auth, from, []string{toEmail}, msg)
	if err != nil {
		fmt.Printf("❌ Failed to send test email to %s: %v\n", toEmail, err)
	} else {
		fmt.Printf("✅ Test email successfully sent to %s\n", toEmail)
	}
}

// ==========================================
// DATABASE (TURSO)
// ==========================================

func connectTurso() (*sql.DB, error) {
	url := os.Getenv("TURSO_DB_URL")
	token := os.Getenv("TURSO_AUTH_TOKEN")
	return sql.Open("libsql", fmt.Sprintf("%s?authToken=%s", url, token))
}

func loadState(db *sql.DB) (SessionState, error) {
	var s SessionState
	var activeInt int
	query := `SELECT ext_id, guid, active, lat, lon, alt, park FROM session_state WHERE id = 'garmin_primary'`
	err := db.QueryRow(query).Scan(&s.ExtID, &s.GUID, &activeInt, &s.Lat, &s.Lon, &s.Alt, &s.Park)
	s.Active = activeInt == 1
	return s, err
}

func saveState(db *sql.DB, s SessionState) error {
	activeInt := 0
	if s.Active {
		activeInt = 1
	}
	query := `UPDATE session_state SET ext_id=?, guid=?, active=?, lat=?, lon=?, alt=?, park=? WHERE id='garmin_primary'`
	_, err := db.Exec(query, s.ExtID, s.GUID, activeInt, s.Lat, s.Lon, s.Alt, s.Park)
	return err
}

// ==========================================
// MAIN HANDLER
// ==========================================

func handler(ctx context.Context) error {
	// 1. Connect to Turso
	db, err := connectTurso()
	if err != nil {
		log.Fatalf("Turso connection failed: %v", err)
	}
	defer db.Close()

	// 2. Load State
	state, err := loadState(db)
	if err != nil {
		log.Fatalf("Failed to load state: %v", err)
	}

	// 3. IMAP Polling & Email Testing
	fmt.Println("Polling IMAP for commands...")
	emailUser := os.Getenv("EMAIL_USER")
	emailPass := os.Getenv("EMAIL_PASS")

	c, err := client.DialTLS("imap.gmail.com:993", nil)
	if err == nil {
		defer c.Logout()
		if err := c.Login(emailUser, emailPass); err == nil {
			mbox, err := c.Select("INBOX", false)
			if err == nil && mbox.Messages > 0 {
				criteria := imap.NewSearchCriteria()
				criteria.WithoutFlags = []string{imap.SeenFlag}
				uids, err := c.Search(criteria)
				
				if err == nil && len(uids) > 0 {
					seqset := new(imap.SeqSet)
					seqset.AddNum(uids...)
					
					messages := make(chan *imap.Message, 10)
					go func() {
						c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope}, messages)
					}()
					
					for msg := range messages {
						subject := msg.Envelope.Subject
						var senderEmail string
						if len(msg.Envelope.From) > 0 {
							senderEmail = msg.Envelope.From[0].MailboxName + "@" + msg.Envelope.From[0].HostName
						}

						// TEST COMMAND PARSER
						testRegex := regexp.MustCompile(`(?i)update\s+lat:\s*([-\d.]+),\s*long:\s*([-\d.]+)`)
						match := testRegex.FindStringSubmatch(subject)

						if len(match) == 3 {
							fmt.Println("🧪 Test command detected! Fetching immediate weather...")
							testLat, _ := strconv.ParseFloat(match[1], 64)
							testLon, _ := strconv.ParseFloat(match[2], 64)
							
							// Mock elevation & park for the test
							testAlt := 2000 
							testPark := "aoraki-mt-cook"

							// Fetch synchronously for the test
							yrData := fetchYrNo(testLat, testLon, testAlt)
							msData := fetchMetService(testPark)
							avlData := fetchAvalanche(testPark)
							
							finalMsg := fmt.Sprintf("%s | %s | %s", yrData, msData, avlData)
							sendTestEmailReply(senderEmail, finalMsg)

							// Mark email as read
							item := imap.FormatFlagsOp(imap.AddFlags, true)
							flags := []interface{}{imap.SeenFlag}
							c.Store(seqset, item, flags, nil)
							continue 
						}

						// NOTE: To implement the Garmin location update, you would parse 
						// the full body here, update `state.Lat/Lon`, and call `saveState(db, state)`
					}
				}
			}
		}
	} else {
		fmt.Printf("IMAP Connection error: %v\n", err)
	}

	// 4. Netlify Cron Window Check (07:00 or 19:00 NZST)
	loc, _ := time.LoadLocation("Pacific/Auckland")
	now := time.Now().In(loc)
	isBroadcastWindow := (now.Hour() == 7 || now.Hour() == 19) && now.Minute() < 5

	if state.Active && isBroadcastWindow {
		fmt.Println("Broadcast window active! Fetching weather concurrently...")

		var yrData, msData, avlData string
		var wg sync.WaitGroup
		wg.Add(3)

		// Concurrent fetches keep us well under the 10-second Lambda limit
		go func() { defer wg.Done(); yrData = fetchYrNo(state.Lat, state.Lon, state.Alt) }()
		go func() { defer wg.Done(); msData = fetchMetService(state.Park) }()
		go func() { defer wg.Done(); avlData = fetchAvalanche(state.Park) }()
		
		wg.Wait()

		finalMsg := fmt.Sprintf("%s | %s | %s", yrData, msData, avlData)
		sendToGarmin(finalMsg, state.ExtID, state.GUID)
	} else {
		fmt.Println("No broadcast needed at this time.")
	}

	return nil
}

func main() {
	lambda.Start(handler)
}
