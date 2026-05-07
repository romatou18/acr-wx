package forecast

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const UserAgent = "AlpineWeatherBot/1.0 (contact: wx.acr.apps@gmail.com)"

type ParkInfo struct {
	Lat    float64
	Lon    float64
	NzaaID int
	MSSlug string
}

var Parks = map[string]ParkInfo{
	// Canterbury / Southern Alps
	"arthurs-pass":         {Lat: -42.94, Lon: 171.56, NzaaID: 4,  MSSlug: "arthurs-pass"},
	"craigieburn":          {Lat: -43.13, Lon: 171.71, NzaaID: 5,  MSSlug: "canterbury-high-country"},
	"mt-hutt":              {Lat: -43.49, Lon: 171.49, NzaaID: 6,  MSSlug: "arthurs-pass"},
	"aoraki-mt-cook":       {Lat: -43.73, Lon: 170.09, NzaaID: 7,  MSSlug: "aoraki-mt-cook"},
	"westland-tai-poutini": {Lat: -43.41, Lon: 170.18, NzaaID: 7,  MSSlug: "aoraki-mt-cook"},
	"two-thumbs":           {Lat: -43.87, Lon: 170.80, NzaaID: 9,  MSSlug: "aoraki-mt-cook"},
	"ohau":                 {Lat: -44.23, Lon: 169.87, NzaaID: 8,  MSSlug: "aoraki-mt-cook"},
	// Otago / Southland
	"mt-aspiring":          {Lat: -44.39, Lon: 168.72, NzaaID: 15, MSSlug: "mt-aspiring"},
	"wanaka":               {Lat: -44.70, Lon: 169.15, NzaaID: 11, MSSlug: "mt-aspiring"},
	"queenstown":           {Lat: -45.03, Lon: 168.66, NzaaID: 10, MSSlug: "mt-aspiring"},
	"fiordland":            {Lat: -45.41, Lon: 167.72, NzaaID: 12, MSSlug: "fiordland"},
	// Nelson / Marlborough / West Coast
	"nelson-lakes":         {Lat: -41.90, Lon: 172.68, NzaaID: 13, MSSlug: "nelson-lakes"},
	"kahurangi":            {Lat: -41.10, Lon: 172.40, NzaaID: 0,  MSSlug: "kahurangi"},
	"paparoa":              {Lat: -42.10, Lon: 171.37, NzaaID: 0,  MSSlug: "paparoa"},
	// North Island
	"tongariro":            {Lat: -39.30, Lon: 175.57, NzaaID: 1,  MSSlug: "tongariro"},
	"egmont":               {Lat: -39.30, Lon: 174.06, NzaaID: 2,  MSSlug: "egmont"},
}

var avlDangerSuffix = map[int]string{
	1: "LOW",
	2: "MODR",
	3: "CONS",
	4: "HIGH",
	5: "EXTR",
}

var httpClient = &http.Client{Timeout: 5 * time.Second}

// BuildReport concurrently fetches yr.no, MetService, and avalanche data for
// the given coordinates and returns the compressed satellite-ready string.
func BuildReport(lat, lon float64, alt int, park string) string {
	var yr, ms, avl string
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); yr = FetchYrNo(lat, lon, alt) }()
	go func() { defer wg.Done(); ms = FetchMetService(park) }()
	go func() { defer wg.Done(); avl = FetchAvalanche(park) }()
	wg.Wait()
	return fmt.Sprintf("%s | %s | %s", yr, ms, avl)
}

// BuildAllReports fetches the forecast for every registered park concurrently
// and returns one report per line, sorted alphabetically by park slug.
func BuildAllReports() string {
	type result struct {
		slug   string
		report string
	}

	results := make([]result, 0, len(Parks))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for slug, info := range Parks {
		wg.Add(1)
		go func(slug string, info ParkInfo) {
			defer wg.Done()
			alt := GetElevation(info.Lat, info.Lon)
			report := BuildReport(info.Lat, info.Lon, alt, slug)
			mu.Lock()
			results = append(results, result{slug, report})
			mu.Unlock()
		}(slug, info)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].slug < results[j].slug })

	var sb strings.Builder
	for _, r := range results {
		sb.WriteString(r.slug + ": " + r.report + "\n")
	}
	return strings.TrimSpace(sb.String())
}

func FetchYrNo(lat, lon float64, alt int) string {
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
	d1Limit := minInt(24, len(ts))
	d2Limit := minInt(48, len(ts))
	for i := 0; i < d1Limit; i++ {
		d1Precip += ts[i].Data.Next1Hours.Details.Precip
	}
	for i := d1Limit; i < d2Limit; i++ {
		d2Precip += ts[i].Data.Next1Hours.Details.Precip
	}

	return fmt.Sprintf("YR T:%dC D1:%dmm D2:%dmm", temp, int(math.Round(d1Precip)), int(math.Round(d2Precip)))
}

func FetchMetService(park string) string {
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
		txt = CompressMetServiceText(rawTxt)

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
	shortPark := MetServiceShortCode(park)

	return fmt.Sprintf("MS(%s) D1 %s W1k:%s 2k:%s 3k:%s | D2 %s W1k:%s 2k:%s 3k:%s",
		shortPark, d1Txt, d1W1, d1W2, d1W3, d2Txt, d2W1, d2W2, d2W3)
}

func FetchAvalanche(parkSlug string) string {
	parkInfo, ok := Parks[parkSlug]
	if !ok || parkInfo.NzaaID == 0 {
		return "AVL:-"
	}

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

func GetClosestPark(lat, lon float64) string {
	closest := "arthurs-pass"
	minDist := math.Inf(1)
	for slug, coords := range Parks {
		dist := math.Hypot(lat-coords.Lat, lon-coords.Lon)
		if dist < minDist {
			minDist = dist
			closest = slug
		}
	}
	return closest
}

func GetElevation(lat, lon float64) int {
	u := fmt.Sprintf("https://api.open-meteo.com/v1/elevation?latitude=%f&longitude=%f", lat, lon)
	resp, err := httpClient.Get(u)
	if err != nil {
		return 2000
	}
	defer resp.Body.Close()

	var result struct {
		Elevation []float64 `json:"elevation"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && len(result.Elevation) > 0 {
		return int(result.Elevation[0])
	}
	return 2000
}

func CompressMetServiceText(text string) string {
	replacements := map[string]string{
		"Partly cloudy": "PrtlyCldy", "Mostly cloudy": "MstlyCldy", "possible": "possib", "occasional": "occas.",
		"isolated showers": "IsoShwrs", "scattered showers": "SctShwrs", "scattered rain": "SctRain",
		"heavy rain": "HvyRain", "falling as snow": "Snow", "showers": "Shwrs", "isolated": "iso", "metre": "mtr", "metres": "mtrs",
		"developing": "dev", "morning": "AM", "afternoon": "PM", "evening": "Eve",
		"Snow possible above":                    "SnowPossibAbov",
		"heavy falls":                            "heavyFalls",
		"heavy falls this Evening":               "heavyFallsEvening",
		"a few showers mainly from low altitude": "fewShowersMainlyInLowAlt",
		"Rain with heavy falls":                  "Rain/heavyFalls",
		"and possible thunderstorm":              "+possibThunderStorm",
		"Fine": "Clear", "turning to": "then", "easing": "easing", "with": "w/", "and": "+",
	}

	title := cases.Title(language.English)
	for old, newStr := range replacements {
		text = strings.ReplaceAll(text, old, newStr)
		text = strings.ReplaceAll(text, title.String(old), newStr)
	}

	text = strings.Join(strings.Fields(text), " ")
	const maxFcst = 45
	if len(text) > maxFcst {
		text = strings.TrimSpace(text[:maxFcst])
	}
	return text
}

func MetServiceShortCode(parkKey string) string {
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

func metServiceSlug(parkKey string) string {
	if info, ok := Parks[parkKey]; ok && info.MSSlug != "" {
		return info.MSSlug
	}
	return parkKey
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
