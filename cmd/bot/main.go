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

	_ "time/tzdata"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// ==========================================
// CONFIGURATION & STRUCTS
// ==========================================
const UserAgent = "AlpineWeatherBot/1.0 (contact: wx.acr.apps@gmail.com)"

type SessionState struct {
	ExtID         string
	GUID          string
	Active        bool
	Lat           float64
	Lon           float64
	Alt           int
	Park          string
	LastFetch     int64 // Unix timestamp of the last successful weather fetch (IMAP trigger or routine broadcast)
	LastRoutineNZ string // NZ wall-clock slot "20060102-07" / "20060102-19" after a 07:00/19:00 routine broadcast (dedupe)
}

type ParkInfo struct {
	Lat    float64
	Lon    float64
	NzaaID int
	// MSSlug is the MetService "national-parks" path segment (see /mountains-and-parks/national-parks/...).
	// Several of our geofence keys are not valid MetService slugs; empty means use the map key.
	MSSlug string
}

var PARKS = map[string]ParkInfo{
	"arthurs-pass":         {Lat: -42.94, Lon: 171.56, NzaaID: 4, MSSlug: "arthurs-pass"},
	"craigieburn":          {Lat: -43.13, Lon: 171.71, NzaaID: 5, MSSlug: "canterbury-high-country"},
	"aoraki-mt-cook":       {Lat: -43.73, Lon: 170.09, NzaaID: 7, MSSlug: "aoraki-mt-cook"},
	"westland-tai-poutini": {Lat: -43.41, Lon: 170.18, NzaaID: 7, MSSlug: "aoraki-mt-cook"},
	"mt-aspiring":          {Lat: -44.39, Lon: 168.72, NzaaID: 15, MSSlug: "mt-aspiring"},
	"nelson-lakes":         {Lat: -41.90, Lon: 172.68, NzaaID: 13, MSSlug: "nelson-lakes"},
}

func metServiceSlug(parkKey string) string {
	if info, ok := PARKS[parkKey]; ok && info.MSSlug != "" {
		return info.MSSlug
	}
	return parkKey
}

// 3-letter code for MS(...) in the satellite payload (README examples: ART, AOR).
func metServiceShortCode(parkKey string) string {
	parts := strings.Split(parkKey, "-")
	var pick string
	for _, seg := range parts {
		if len(seg) >= 3 {
			pick = seg
			break
		}
	}
	if pick == "" {
		pick = strings.Join(parts, "")
	}
	u := strings.ToUpper(pick)
	if len(u) > 3 {
		u = u[:3]
	}
	return u
}

// NZAA / MetService use 1–5 avalanche danger; we keep a 4-letter satellite suffix (README examples).
var avlDangerSuffix = map[int]string{
	1: "LOW",
	2: "MODR",
	3: "CONS",
	4: "HIGH",
	5: "EXTR",
}

func atoiKmhToken(s string) int {
	if s == "" || s == "??" {
		return -1
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

// MetService often omits a 3000 m row even though the product is "3-tier"; extrapolate from 1k/2k when missing.
func estimateWind3000m(w1k, w2k string) string {
	w1 := atoiKmhToken(w1k)
	w2 := atoiKmhToken(w2k)
	if w2 < 0 {
		return "??"
	}
	if w1 < 0 {
		b := w2 / 5
		if b < 8 {
			b = 8
		}
		if b > 25 {
			b = 25
		}
		est := w2 + b
		if est > 150 {
			est = 150
		}
		return strconv.Itoa(est)
	}
	delta := w2 - w1
	est := w2 + delta
	if est < w2+5 {
		est = w2 + 5
	}
	if est > w2+55 {
		est = w2 + 55
	}
	if est > 150 {
		est = 150
	}
	return strconv.Itoa(est)
}

func windHeightMetres(wObj map[string]any) int {
	v, ok := wObj["heightMetres"]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	default:
		return 0
	}
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
	// MetService is a SPA; the HTML doesn't contain forecast data. Use the backing JSON endpoint.
	msSlug := metServiceSlug(park)
	targetURL := fmt.Sprintf("https://www.metservice.com/publicData/webdata/mountains-and-parks/national-parks/%s", msSlug)

	req, _ := http.NewRequest("GET", targetURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "MS:Err"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "MS:Err"
	}

	var payload map[string]any
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return "MS:JSON_Err"
	}

	// Extract the daily forecast list:
	// layout.secondary.slots.major.modules[0].days[i].forecast.{forecast,wind[]}
	getObj := func(m map[string]any, key string) (map[string]any, bool) {
		v, ok := m[key]
		if !ok {
			return nil, false
		}
		out, ok := v.(map[string]any)
		return out, ok
	}
	getArr := func(m map[string]any, key string) ([]any, bool) {
		v, ok := m[key]
		if !ok {
			return nil, false
		}
		out, ok := v.([]any)
		return out, ok
	}

	layout, ok := getObj(payload, "layout")
	if !ok {
		return "MS:NoLayout"
	}
	secondary, ok := getObj(layout, "secondary")
	if !ok {
		return "MS:NoLayout"
	}
	slots, ok := getObj(secondary, "slots")
	if !ok {
		return "MS:NoLayout"
	}
	major, ok := getObj(slots, "major")
	if !ok {
		return "MS:NoLayout"
	}
	modules, ok := getArr(major, "modules")
	if !ok || len(modules) == 0 {
		return "MS:NoData"
	}
	firstModule, ok := modules[0].(map[string]any)
	if !ok {
		return "MS:NoData"
	}
	days, ok := getArr(firstModule, "days")
	if !ok || len(days) < 2 {
		return "MS:NoDays"
	}

	parseWindKmh := func(s string) string {
		// Take the maximum km/h value mentioned (e.g. "gale 65 km/h, rising to gale 85 km/h").
		re := regexp.MustCompile(`(\d{2,3})\s*km/h`)
		matches := re.FindAllStringSubmatch(s, -1)
		if len(matches) == 0 {
			return "??"
		}
		maxV := 0
		for _, m := range matches {
			v, err := strconv.Atoi(m[1])
			if err == nil && v > maxV {
				maxV = v
			}
		}
		if maxV == 0 {
			return "??"
		}
		return strconv.Itoa(maxV)
	}

	extractDay := func(day any) (txt, w1, w2, w3 string) {
		w1, w2, w3 = "??", "??", "??"
		dayObj, ok := day.(map[string]any)
		if !ok {
			return "??", w1, w2, w3
		}
		fcAny, ok := dayObj["forecast"]
		if !ok {
			return "??", w1, w2, w3
		}
		fcObj, ok := fcAny.(map[string]any)
		if !ok {
			return "??", w1, w2, w3
		}
		rawTxt, _ := fcObj["forecast"].(string)
		txt = compressMetServiceText(rawTxt)

		if windAny, ok := fcObj["wind"]; ok {
			if windArr, ok := windAny.([]any); ok {
				for _, w := range windArr {
					wObj, ok := w.(map[string]any)
					if !ok {
						continue
					}
					h := windHeightMetres(wObj)
					raw, _ := wObj["forecast"].(string)
					kmh := parseWindKmh(raw)
					switch h {
					case 1000:
						w1 = kmh
					case 2000:
						w2 = kmh
					case 3000:
						w3 = kmh
					}
				}
			}
		}
		if w3 == "??" {
			w3 = estimateWind3000m(w1, w2)
		}
		if txt == "" {
			txt = "??"
		}
		return txt, w1, w2, w3
	}

	d1Txt, d1W1, d1W2, d1W3 := extractDay(days[0])
	d2Txt, d2W1, d2W2, d2W3 := extractDay(days[1])

	shortPark := metServiceShortCode(park)

	return fmt.Sprintf("MS(%s) D1 %s W1k:%s 2k:%s 3k:%s | D2 %s W1k:%s 2k:%s 3k:%s", 
		shortPark, d1Txt, d1W1, d1W2, d1W3, d2Txt, d2W1, d2W2, d2W3)
}

func fetchAvalanche(parkSlug string) string {
	parkInfo, ok := PARKS[parkSlug]
	if !ok {
		return "AVL:??"
	}

	// NZAA site is a Vue SPA; the public JSON API is what the app calls.
	u := fmt.Sprintf("https://www.avalanche.net.nz/api/forecastsearch?region=%d", parkInfo.NzaaID)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", UserAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "AVL:Err"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "AVL:Err"
	}

	var payload struct {
		Forecast struct {
			AltitudeDanger []struct {
				Rating int `json:"rating"`
			} `json:"altitudeDanger"`
			DangerRatingForecast struct {
				Rating int `json:"rating"`
			} `json:"dangerRatingForecast"`
		} `json:"forecast"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "AVL:JSON_Err"
	}

	maxR := -1
	hasInsufficient := false
	for _, band := range payload.Forecast.AltitudeDanger {
		r := band.Rating
		if r == -3 {
			hasInsufficient = true
		}
		if r >= 1 && r <= 5 && r > maxR {
			maxR = r
		}
	}
	if maxR >= 1 {
		suf, ok := avlDangerSuffix[maxR]
		if !ok {
			return "AVL:??"
		}
		return fmt.Sprintf("AVL:%d-%s", maxR, suf)
	}

	dr := payload.Forecast.DangerRatingForecast.Rating
	if dr >= 1 && dr <= 5 {
		suf, ok := avlDangerSuffix[dr]
		if !ok {
			return "AVL:??"
		}
		return fmt.Sprintf("AVL:%d-%s", dr, suf)
	}

	if hasInsufficient {
		return "AVL:-"
	}
	if dr == 0 {
		return "AVL:0-NRAT"
	}
	return "AVL:??"
}

func sendToGarmin(msg, extId, guid string) {
	if len(msg) > 160 { msg = msg[:160] }
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

func getClosestPark(lat, lon float64) string {
	closestPark := "arthurs-pass"
	minDist := math.Inf(1)
	for slug, coords := range PARKS {
		dist := math.Hypot(lat-coords.Lat, lon-coords.Lon)
		if dist < minDist {
			minDist = dist
			closestPark = slug
		}
	}
	return closestPark
}

func getElevation(lat, lon float64) int {
	url := fmt.Sprintf("https://api.open-meteo.com/v1/elevation?latitude=%f&longitude=%f", lat, lon)
	resp, err := httpClient.Get(url)
	if err != nil { return 2000 }
	defer resp.Body.Close()

	var result struct { Elevation []float64 `json:"elevation"` }
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && len(result.Elevation) > 0 {
		return int(result.Elevation[0])
	}
	return 2000
}

func compressMetServiceText(text string) string {
	replacements := map[string]string{
		"Partly cloudy": "PrtlyCldy", "Mostly cloudy": "MstlyCldy",
		"isolated showers": "IsoShwrs", "scattered showers": "SctShwrs",
		"heavy rain": "HvyRain", "falling as snow": "Snow", "showers": "Shwrs",
		"developing": "dev", "morning": "AM", "afternoon": "PM", "evening": "Eve",
		"Fine": "Clear", "turning to": "->", "easing": "eas", "with": "w/",
	}

	title := cases.Title(language.English)
	for old, newStr := range replacements {
		text = strings.ReplaceAll(text, old, newStr)
		text = strings.ReplaceAll(text, title.String(old), newStr)
	}

	text = strings.Join(strings.Fields(text), " ")
	const maxFcst = 45 // keep qualitative line satellite-friendly; full message capped at 160 in sendToGarmin
	if len(text) > maxFcst {
		text = strings.TrimSpace(text[:maxFcst])
	}
	return text
}

func min(a, b int) int {
	if a < b { return a }
	return b
}

func sendTestEmailReply(toEmail, report string) {
	from := os.Getenv("EMAIL_USER")
	pass := os.Getenv("EMAIL_PASS")
	host := "smtp.gmail.com"
	port := "587"

	msg := []byte("To: " + toEmail + "\r\n" +
		"Subject: Alpine Weather Test Report\r\n\r\n" +
		"Here is your simulated Alpine Weather Report:\r\n\n" + report + "\r\n")

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
	dbURL := os.Getenv("TURSO_DB_URL")
	token := os.Getenv("TURSO_AUTH_TOKEN")
	return sql.Open("libsql", fmt.Sprintf("%s?authToken=%s", dbURL, token))
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
		`UPDATE session_state SET ext_id=?, guid=?, active=?, lat=?, lon=?, alt=?, park=?, last_fetch=?, last_routine_nz=? WHERE id='garmin_primary'`,
		s.ExtID, s.GUID, activeInt, s.Lat, s.Lon, s.Alt, s.Park, s.LastFetch, s.LastRoutineNZ,
	)
	if err != nil && strings.Contains(err.Error(), "last_routine") {
		_, err = db.Exec(
			`UPDATE session_state SET ext_id=?, guid=?, active=?, lat=?, lon=?, alt=?, park=?, last_fetch=? WHERE id='garmin_primary'`,
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
	// 1-minute cron may not land exactly on :00; still dedupe with LastRoutineNZ.
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

	state, err := loadState(db)
	if err != nil {
		log.Printf("Warning: Failed to load state (Is DB setup?): %v\n", err)
	}

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
					
					section := &imap.BodySectionName{}
					items := []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}
					
					messages := make(chan *imap.Message, 10)
					go func() {
						c.Fetch(seqset, items, messages)
					}()
					
					for msg := range messages {
						subject := msg.Envelope.Subject
						var senderEmail string
						if len(msg.Envelope.From) > 0 {
							senderEmail = msg.Envelope.From[0].MailboxName + "@" + msg.Envelope.From[0].HostName
						}

						// 1. TEST COMMAND PARSER
						testRegex := regexp.MustCompile(`(?i)update\s+lat:\s*([-\d.]+),\s*long:\s*([-\d.]+)`)
						match := testRegex.FindStringSubmatch(subject)

						if len(match) == 3 {
							fmt.Println("🧪 Test command detected! Fetching immediate weather...")
							testLat, _ := strconv.ParseFloat(match[1], 64)
							testLon, _ := strconv.ParseFloat(match[2], 64)
							
							testPark := getClosestPark(testLat, testLon)
							testAlt := getElevation(testLat, testLon)

							var yrData, msData, avlData string
							var wg sync.WaitGroup
							wg.Add(3)
							go func() { defer wg.Done(); yrData = fetchYrNo(testLat, testLon, testAlt) }()
							go func() { defer wg.Done(); msData = fetchMetService(testPark) }()
							go func() { defer wg.Done(); avlData = fetchAvalanche(testPark) }()
							wg.Wait()

							finalMsg := fmt.Sprintf("%s | %s | %s", yrData, msData, avlData)
							fmt.Printf("📋 Test weather report (%d chars): %s\n", len(finalMsg), finalMsg)
							sendTestEmailReply(senderEmail, finalMsg)

							item := imap.FormatFlagsOp(imap.AddFlags, true)
							flags := []interface{}{imap.SeenFlag}
							c.Store(seqset, item, flags, nil)
							continue 
						}

						// 2. GARMIN COMMAND PARSER
						r := msg.GetBody(section)
						if r == nil { continue }
						
						bodyBytes, _ := io.ReadAll(r)
						bodyStr := string(bodyBytes)

						garminDirty := false

						sessionMatch := regexp.MustCompile(`extId=([^&]+)&guid=([^&]+)`).FindStringSubmatch(bodyStr)
						if len(sessionMatch) == 3 {
							state.ExtID = sessionMatch[1]
							state.GUID = sessionMatch[2]
							garminDirty = true
						}

						upperBody := strings.ToUpper(bodyStr)
						if strings.Contains(upperBody, "START") {
							state.Active = true
							garminDirty = true
						} else if strings.Contains(upperBody, "STOP") {
							state.Active = false
							sendToGarmin("Server: Updates Paused.", state.ExtID, state.GUID)
							garminDirty = true
						}

						coordMatch := regexp.MustCompile(`Lat:\s*([-\d.]+)\s*Lon:\s*([-\d.]+)`).FindStringSubmatch(bodyStr)
						if len(coordMatch) == 3 {
							newLat, _ := strconv.ParseFloat(coordMatch[1], 64)
							newLon, _ := strconv.ParseFloat(coordMatch[2], 64)

							newPark := getClosestPark(newLat, newLon)
							locationChanged := newPark != state.Park
							isStale := time.Now().Unix()-state.LastFetch > (12 * 3600)
							isUpdateCmd := strings.Contains(upperBody, "UPDATE")

							state.Lat = newLat
							state.Lon = newLon
							state.Park = newPark
							state.Alt = getElevation(newLat, newLon)
							garminDirty = true

							if state.Active && (locationChanged || isStale || isUpdateCmd) {
								fmt.Println("🚀 Immediate fetch triggered! (New location, stale data, or UPDATE cmd)")

								var yrData, msData, avlData string
								var wg sync.WaitGroup
								wg.Add(3)
								go func() { defer wg.Done(); yrData = fetchYrNo(state.Lat, state.Lon, state.Alt) }()
								go func() { defer wg.Done(); msData = fetchMetService(state.Park) }()
								go func() { defer wg.Done(); avlData = fetchAvalanche(state.Park) }()
								wg.Wait()

								finalMsg := fmt.Sprintf("%s | %s | %s", yrData, msData, avlData)
								sendToGarmin(finalMsg, state.ExtID, state.GUID)

								state.LastFetch = time.Now().Unix()
							}
						}

						if garminDirty {
							if err := saveState(db, state); err != nil {
								log.Printf("save session state: %v", err)
							}
						}

						item := imap.FormatFlagsOp(imap.AddFlags, true)
						flags := []interface{}{imap.SeenFlag}
						c.Store(seqset, item, flags, nil)
					}
				}
			}
		}
	} else {
		fmt.Printf("IMAP Connection error: %v\n", err)
	}

	// 4. ROUTINE BROADCAST CHECK (07:00 / 19:00 NZ wall time — Pacific/Auckland, NZST/NZDT)
	loc, tzErr := time.LoadLocation("Pacific/Auckland")
	if tzErr != nil {
		log.Printf("Pacific/Auckland timezone: %v", tzErr)
		fmt.Println("No scheduled broadcast needed at this time.")
	} else {
		now := time.Now().In(loc)
		if shouldRoutineBroadcast(state, now) {
			fmt.Println("🌅 Broadcast window active! Fetching routine weather...")
			slot := routineBroadcastSlot(now)

			var yrData, msData, avlData string
			var wg sync.WaitGroup
			wg.Add(3)

			go func() { defer wg.Done(); yrData = fetchYrNo(state.Lat, state.Lon, state.Alt) }()
			go func() { defer wg.Done(); msData = fetchMetService(state.Park) }()
			go func() { defer wg.Done(); avlData = fetchAvalanche(state.Park) }()
			wg.Wait()

			finalMsg := fmt.Sprintf("%s | %s | %s", yrData, msData, avlData)
			sendToGarmin(finalMsg, state.ExtID, state.GUID)

			state.LastFetch = time.Now().Unix()
			state.LastRoutineNZ = slot
			if err := saveState(db, state); err != nil {
				log.Printf("save state after broadcast: %v", err)
			}
		} else {
			fmt.Println("No scheduled broadcast needed at this time.")
		}
	}

	return nil
}

func main() {
	// README local test: run once (not the Lambda runtime loop). Scheduled deploys use lambda.Start.
	if os.Getenv("LOCAL_WEATHER_BOT") == "1" {
		if err := handler(context.Background()); err != nil {
			log.Fatal(err)
		}
		return
	}
	lambda.Start(handler)
}
