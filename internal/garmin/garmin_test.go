package garmin

import (
	"os"
	"testing"
)

// TestAnalyzePage_RealFixture runs AnalyzePage over the committed real (sanitized)
// explore.garmin.com page — the same fixture the bot's offline loop test uses — and
// asserts a clean PASS. This guards the shared parser against accidental breakage.
func TestAnalyzePage_RealFixture(t *testing.T) {
	html, err := os.ReadFile("../../cmd/bot/testdata/garmin_message_page.html")
	if err != nil {
		t.Skipf("golden fixture not available: %v", err)
	}
	r := AnalyzePage(string(html))
	if !r.OK {
		t.Fatalf("real page should parse OK, got %+v", r)
	}
	if r.Guid != "TEST-guid-0000-0000-000000000001" {
		t.Errorf("Guid = %q", r.Guid)
	}
	if r.MessageId != "999000111" {
		t.Errorf("MessageId = %q", r.MessageId)
	}
	if !r.CoordsFound || r.Lat != -43.730000 || r.Lon != 170.090000 {
		t.Errorf("coords = (%v,%v) found=%v, want (-43.73,170.09,true)", r.Lat, r.Lon, r.CoordsFound)
	}
}

func TestAnalyzePage_NoFix(t *testing.T) {
	// Guid/MessageId present, but Locations report 0,0 (app message, no GPS).
	page := `<input name="Guid" type="hidden" value="g-123">
		<input name="MessageId" type="hidden" value="42">
		<script>var m = {"Locations":[{"Latitude":0,"Longitude":0}]};</script>`
	r := AnalyzePage(page)
	if !r.OK {
		t.Errorf("guid+messageId present should be OK regardless of coords: %+v", r)
	}
	if r.CoordsFound {
		t.Errorf("0,0 must be reported as no fix, got found=true (%v,%v)", r.Lat, r.Lon)
	}
	if len(r.Issues) == 0 {
		t.Error("a no-fix page should record an informational issue")
	}
}

func TestAnalyzePage_NoReplyFields(t *testing.T) {
	r := AnalyzePage(`<html><body>not a message page</body></html>`)
	if r.OK {
		t.Errorf("a page without Guid/MessageId must FAIL: %+v", r)
	}
	if r.GuidFound || r.MessageIdFound {
		t.Errorf("nothing should be found: %+v", r)
	}
	if len(r.Issues) < 2 {
		t.Errorf("should flag both missing Guid and MessageId, got %v", r.Issues)
	}
}
