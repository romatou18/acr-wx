package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata"

	"acr-wx/internal/forecast"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func handler(_ context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	path := strings.TrimRight(req.Path, "/")

	// GET /log or /weather-api/log  — usage log
	if path == "/weather-api/log" || path == "/log" {
		return handleLogs(req)
	}

	// GET /debug or /weather-api/debug  — JSON debug logs for the live viewer
	if path == "/weather-api/debug" || path == "/debug" {
		return handleDebug(req)
	}

	// GET /weather-api/all  — report for every registered park
	if strings.HasSuffix(path, "/all") {
		log.Println("GET /weather-api/all")
		return ok(forecast.BuildAllReports()), nil
	}

	// GET /weather-api?lat=<lat>&lon=<lon>
	latStr := req.QueryStringParameters["lat"]
	lonStr := req.QueryStringParameters["lon"]

	if latStr == "" || lonStr == "" {
		return badRequest("missing required query params: lat, lon"), nil
	}
	lat, err := strconv.ParseFloat(latStr, 64)
	if err != nil {
		return badRequest("invalid lat: must be a decimal number"), nil
	}
	lon, err := strconv.ParseFloat(lonStr, 64)
	if err != nil {
		return badRequest("invalid lon: must be a decimal number"), nil
	}
	if lat < -90 || lat > 90 {
		return badRequest("lat out of range [-90, 90]"), nil
	}
	if lon < -180 || lon > 180 {
		return badRequest("lon out of range [-180, 180]"), nil
	}

	park := forecast.GetClosestPark(lat, lon)
	alt := forecast.GetElevation(lat, lon)
	report := forecast.BuildReport(lat, lon, alt, park)
	log.Printf("GET /weather-api lat=%s lon=%s park=%s alt=%d → %d chars", latStr, lonStr, park, alt, len(report))
	return ok(report), nil
}

func handleLogs(req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	if key := os.Getenv("LOGS_KEY"); key != "" && req.QueryStringParameters["key"] != key {
		return events.APIGatewayProxyResponse{
			StatusCode: 401,
			Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
			Body:       "unauthorized — set ?key=<LOGS_KEY>\n",
		}, nil
	}

	db, err := connectTurso()
	if err != nil {
		return serverError(fmt.Sprintf("db connect: %v", err)), nil
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT ts, source, client, command, lat, lon, park FROM request_log ORDER BY ts DESC LIMIT 200`,
	)
	if err != nil {
		return serverError(fmt.Sprintf("query: %v", err)), nil
	}
	defer rows.Close()

	loc, _ := time.LoadLocation("Pacific/Auckland")

	const line = "────────────────────────────────────────────────────────────────────────────────────\n"
	var sb strings.Builder
	sb.WriteString("ACR Alpine Weather — Usage Log\n")
	sb.WriteString(line)
	sb.WriteString(fmt.Sprintf("%-20s %-8s %-34s %-8s %s\n", "TIME (NZT)", "SOURCE", "CLIENT", "CMD", "LOCATION"))
	sb.WriteString(line)

	count := 0
	for rows.Next() {
		var ts int64
		var source, client, command, park string
		var lat, lon float64
		if err := rows.Scan(&ts, &source, &client, &command, &lat, &lon, &park); err != nil {
			continue
		}
		t := time.Unix(ts, 0).In(loc)
		locStr := "—"
		if lat != 0 || lon != 0 {
			locStr = fmt.Sprintf("%s (%.4f, %.4f)", park, lat, lon)
		}
		sb.WriteString(fmt.Sprintf("%-20s %-8s %-34s %-8s %s\n",
			t.Format("2006-01-02 15:04"),
			source, client, command, locStr))
		count++
	}

	if count == 0 {
		sb.WriteString("  (no requests logged yet)\n")
	}
	sb.WriteString(line)
	sb.WriteString(fmt.Sprintf("%d request(s)\n", count))

	return ok(sb.String()), nil
}

// handleDebug serves the bot's captured debug logs as JSON for the live viewer
// at /debug.html. Supports incremental polling via ?after=<id> (returns only
// rows newer than that id); with no/zero after it returns the most recent 300
// lines in chronological order. Protected by the same LOGS_KEY as /log.
func handleDebug(req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	if key := os.Getenv("LOGS_KEY"); key != "" && req.QueryStringParameters["key"] != key {
		return jsonResp(401, `{"error":"unauthorized — set ?key=<LOGS_KEY>"}`), nil
	}

	db, err := connectTurso()
	if err != nil {
		return jsonResp(500, fmt.Sprintf(`{"error":%q}`, "db connect: "+err.Error())), nil
	}
	defer db.Close()

	// Tolerate a fresh DB where the bot hasn't created the table yet.
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS debug_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
		seq INTEGER NOT NULL, run TEXT NOT NULL, msg TEXT NOT NULL)`)

	after, _ := strconv.ParseInt(req.QueryStringParameters["after"], 10, 64)

	var rows *sql.Rows
	if after > 0 {
		rows, err = db.Query(`SELECT id, ts, run, msg FROM debug_log WHERE id > ? ORDER BY id ASC LIMIT 1000`, after)
	} else {
		rows, err = db.Query(`SELECT id, ts, run, msg FROM (SELECT id, ts, run, msg FROM debug_log ORDER BY id DESC LIMIT 300) ORDER BY id ASC`)
	}
	if err != nil {
		return jsonResp(500, fmt.Sprintf(`{"error":%q}`, "query: "+err.Error())), nil
	}
	defer rows.Close()

	type entry struct {
		ID  int64  `json:"id"`
		TS  int64  `json:"ts"`
		Run string `json:"run"`
		Msg string `json:"msg"`
	}
	out := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.TS, &e.Run, &e.Msg); err != nil {
			continue
		}
		out = append(out, e)
	}

	b, err := json.Marshal(out)
	if err != nil {
		return jsonResp(500, fmt.Sprintf(`{"error":%q}`, "marshal: "+err.Error())), nil
	}
	return jsonResp(200, string(b)), nil
}

func jsonResp(code int, body string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: code,
		Headers: map[string]string{
			"Content-Type":                "application/json; charset=utf-8",
			"Access-Control-Allow-Origin": "*",
			"Cache-Control":               "no-store",
		},
		Body: body,
	}
}

func connectTurso() (*sql.DB, error) {
	dbURL := os.Getenv("TURSO_DB_URL")
	token := os.Getenv("TURSO_AUTH_TOKEN")
	return sql.Open("libsql", fmt.Sprintf("%s?authToken=%s", dbURL, token))
}

func ok(body string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       body,
	}
}

func badRequest(msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusBadRequest,
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       fmt.Sprintf("error: %s\nusage: GET /weather-api?lat=<lat>&lon=<lon>  or  GET /weather-api/all\n", msg),
	}
}

func serverError(msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusInternalServerError,
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       "error: " + msg + "\n",
	}
}

func main() {
	if os.Getenv("LOCAL_WEATHER_API") == "1" {
		handle := func(w http.ResponseWriter, r *http.Request) {
			resp, _ := handler(r.Context(), events.APIGatewayProxyRequest{
				Path: r.URL.Path,
				QueryStringParameters: map[string]string{
					"lat":   r.URL.Query().Get("lat"),
					"lon":   r.URL.Query().Get("lon"),
					"key":   r.URL.Query().Get("key"),
					"after": r.URL.Query().Get("after"),
				},
			})
			for k, v := range resp.Headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(resp.StatusCode)
			fmt.Fprint(w, resp.Body)
		}
		http.HandleFunc("/weather-api/all", handle)
		http.HandleFunc("/weather-api/log", handle)
		http.HandleFunc("/weather-api/debug", handle)
		http.HandleFunc("/log", handle)
		http.HandleFunc("/debug", handle)
		http.HandleFunc("/weather-api", handle)
		// Serve the static pages (index.html, debug.html) so the viewer works locally.
		http.Handle("/", http.FileServer(http.Dir("public")))
		log.Println("Listening on :9090  (try http://localhost:9090/debug.html)")
		log.Fatal(http.ListenAndServe(":9090", nil))
		return
	}
	lambda.Start(handler)
}
