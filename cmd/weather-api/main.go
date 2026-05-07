package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"acr-wx/internal/forecast"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

// handler responds to GET /weather-api?lat=<lat>&lon=<lon>
// with the same compressed report that is sent to the Garmin device.
func handler(_ context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
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

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       report,
	}, nil
}

func badRequest(msg string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusBadRequest,
		Headers:    map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:       fmt.Sprintf("error: %s\nusage: GET /weather-api?lat=<lat>&lon=<lon>\n", msg),
	}
}

func main() {
	if os.Getenv("LOCAL_WEATHER_API") == "1" {
		http.HandleFunc("/weather-api", func(w http.ResponseWriter, r *http.Request) {
			lat := r.URL.Query().Get("lat")
			lon := r.URL.Query().Get("lon")
			resp, _ := handler(r.Context(), events.APIGatewayProxyRequest{
				QueryStringParameters: map[string]string{"lat": lat, "lon": lon},
			})
			w.Header().Set("Content-Type", resp.Headers["Content-Type"])
			w.WriteHeader(resp.StatusCode)
			fmt.Fprint(w, resp.Body)
		})
		port := ":9090"
		log.Printf("Listening on %s", port)
		log.Fatal(http.ListenAndServe(port, nil))
		return
	}
	lambda.Start(handler)
}
