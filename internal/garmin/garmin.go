// Package garmin holds the canonical parsers for an explore.garmin.com inReach
// message page. They are shared by the weather-bot (which scrapes the page to
// reply to a device) and the weather-api (which exposes an on-demand "does the
// real page still parse?" test). Keeping a single implementation guarantees the
// test validates exactly what the bot relies on — if Garmin changes the page,
// both break together and the test surfaces it.
package garmin

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// reShortlinkLatLon matches the sender fix embedded in the message page JSON:
//
//	"Locations":[{ ... "Latitude":<lat>,"Longitude":<lon> ... }]
var reShortlinkLatLon = regexp.MustCompile(`"Latitude":\s*(-?\d+(?:\.\d+)?)\s*,\s*"Longitude":\s*(-?\d+(?:\.\d+)?)`)

// ParseReplyFields pulls the reply target out of the message page. Garmin
// exposes it as hidden inputs: <input name="Guid" value="..."> and
// <input name="MessageId" value="...">.
func ParseReplyFields(html string) (guid, messageId string) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", ""
	}
	guid, _ = doc.Find(`input[name="Guid"]`).Attr("value")
	messageId, _ = doc.Find(`input[name="MessageId"]`).Attr("value")
	return strings.TrimSpace(guid), strings.TrimSpace(messageId)
}

// ParseShortlinkCoords returns the first valid, non-zero coordinate pair found
// in the inReach message page HTML. ok=false means no usable fix was present
// (app-relayed messages with no GPS fix report 0,0).
func ParseShortlinkCoords(pageHTML string) (lat, lon float64, ok bool) {
	for _, m := range reShortlinkLatLon.FindAllStringSubmatch(pageHTML, -1) {
		la, e1 := strconv.ParseFloat(m[1], 64)
		lo, e2 := strconv.ParseFloat(m[2], 64)
		if e1 != nil || e2 != nil {
			continue
		}
		if la == 0 && lo == 0 {
			continue // no GPS fix
		}
		if la < -90 || la > 90 || lo < -180 || lo > 180 {
			continue
		}
		return la, lo, true
	}
	return 0, 0, false
}

// Report is the verdict of analysing an uploaded message page. OK is true when
// the page yields the Guid/MessageId pair the bot needs to post a reply. Coords
// are optional — an app-relayed message legitimately reports no fix — so a
// missing fix is noted in Issues, not a failure.
type Report struct {
	OK             bool     `json:"ok"`
	GuidFound      bool     `json:"guidFound"`
	Guid           string   `json:"guid"`
	MessageIdFound bool     `json:"messageIdFound"`
	MessageId      string   `json:"messageId"`
	CoordsFound    bool     `json:"coordsFound"`
	Lat            float64  `json:"lat"`
	Lon            float64  `json:"lon"`
	Bytes          int      `json:"bytes"`
	Issues         []string `json:"issues"`
}

// AnalyzePage runs the bot's page parsers over an uploaded message page and
// reports whether it still yields a usable reply target. This is the exact code
// path the bot uses, so a PASS here means the bot can reply; a FAIL means Garmin
// changed the page and replies would break.
func AnalyzePage(html string) Report {
	guid, messageId := ParseReplyFields(html)
	lat, lon, coordsOK := ParseShortlinkCoords(html)

	r := Report{
		Guid:           guid,
		GuidFound:      guid != "",
		MessageId:      messageId,
		MessageIdFound: messageId != "",
		CoordsFound:    coordsOK,
		Lat:            lat,
		Lon:            lon,
		Bytes:          len(html),
		Issues:         []string{},
	}
	r.OK = r.GuidFound && r.MessageIdFound

	if !r.GuidFound {
		r.Issues = append(r.Issues, `Guid hidden input not found (input[name="Guid"]) — page layout may have changed`)
	}
	if !r.MessageIdFound {
		r.Issues = append(r.Issues, `MessageId hidden input not found (input[name="MessageId"]) — page layout may have changed`)
	}
	if !r.CoordsFound {
		r.Issues = append(r.Issues, "no GPS fix in Locations[] (0,0 or absent) — acceptable for app-relayed messages; the bot falls back to last-known location")
	}
	return r
}
