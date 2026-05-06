package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// SessionState mirrors our Turso SQLite table
type SessionState struct {
	ExtID  string
	GUID   string
	Active bool
	Lat    float64
	Lon    float64
	Alt    int
	Park   string
}

// ---------------------------------------------------------
// DATABASE LOGIC (TURSO)
// ---------------------------------------------------------
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

// ---------------------------------------------------------
// FORECAST FETCHERS (Stubbed for brevity - insert logic here)
// ---------------------------------------------------------
func fetchYrNo(lat, lon float64, alt int) string {
	// Insert the yr.no HTTP request logic here
	return "YR T:-10C D1:15mm D2:0mm"
}

func fetchMetService(park string) string {
	// Insert the MetService scraper logic here
	return "MS(AOR) D1 Snow W1k:20 2k:45 3k:80"
}

func fetchAvalanche(park string) string {
	// Insert Avalanche NZ logic here
	return "AVL:4-HIGH"
}

func sendToGarmin(msg, extId, guid string) {
	// Insert Garmin POST request logic here
	fmt.Printf("Sending to Garmin: %s\n", msg)
}

// ---------------------------------------------------------
// MAIN SERVERLESS HANDLER
// ---------------------------------------------------------
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

	// 3. Poll Email (IMAP)
	// Execute your IMAP checking logic here.
	// If a new email is found, update `state.Lat`, `state.ExtID`, etc., and run `saveState(db, state)`
	fmt.Println("Polling IMAP...")

	// 4. Time Check for Broadcast (Netlify cron triggers every 5 mins)
	// We check if the current time is exactly the 07:00 or 19:00 window in NZST
	loc, _ := time.LoadLocation("Pacific/Auckland")
	now := time.Now().In(loc)
	
	isBroadcastWindow := (now.Hour() == 7 || now.Hour() == 19) && now.Minute() < 5

	if state.Active && isBroadcastWindow {
		fmt.Println("Broadcast window active! Fetching weather concurrently...")

		var yrData, msData, avlData string
		var wg sync.WaitGroup
		wg.Add(3)

		// Fetch APIs concurrently to ensure we stay under the 10-second Lambda timeout
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
	// Netlify injects the cron trigger here
	lambda.Start(handler)
}
