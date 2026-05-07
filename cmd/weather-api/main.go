package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"acr-wx/internal/forecast"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

func handler(_ context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	// GET /weather-api/all  — report for every registered park
	if strings.HasSuffix(strings.TrimRight(req.Path, "/"), "/all") {
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

func main() {
	if os.Getenv("LOCAL_WEATHER_API") == "1" {
		handle := func(w http.ResponseWriter, r *http.Request) {
			resp, _ := handler(r.Context(), events.APIGatewayProxyRequest{
				Path:                  r.URL.Path,
				QueryStringParameters: map[string]string{"lat": r.URL.Query().Get("lat"), "lon": r.URL.Query().Get("lon")},
			})
			w.Header().Set("Content-Type", resp.Headers["Content-Type"])
			w.WriteHeader(resp.StatusCode)
			fmt.Fprint(w, resp.Body)
		}
		http.HandleFunc("/weather-api/all", handle)
		http.HandleFunc("/weather-api", handle)
		log.Println("Listening on :9090")
		log.Fatal(http.ListenAndServe(":9090", nil))
		return
	}
	lambda.Start(handler)
}
