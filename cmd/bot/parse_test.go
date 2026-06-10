package main

import (
	"os"
	"strings"
	"testing"
)

// realGarminUpdateBody is the exact decoded text/plain body of a real Garmin
// inReach "Update" email (captured 2026-06-10). Used as a golden fixture so the
// parser stays faithful to Garmin's actual format.
const realGarminUpdateBody = `Update

View the location or send a reply to Christchurch ACR 01:
https://inreachlink.com/g4plcdeFSKxxEMQbrGbVlbQ

Christchurch ACR 01 sent this message from: Lat -44.988456 Lon 168.904967

Do not reply directly to this message.

This message was sent to you using the inReach two-way satellite
communicator with GPS. To learn more, visit
http://explore.garmin.com/inreach.
`

// These tests run with no network and no IMAP — they exercise the pure parsing
// and routing helpers so you can see exactly how a given email (e.g. "UPDATE")
// is interpreted. Run: go test ./cmd/bot/

func TestIsGarminSender(t *testing.T) {
	cases := map[string]bool{
		"no.reply@garmin.com":        true,
		"service@inreach.garmin.com": true,
		"foo@inreachlink.com":        true,
		"romain@keaaerospace.com":    false,
		"someone@gmail.com":          false,
	}
	for addr, want := range cases {
		if got := isGarminSender(addr); got != want {
			t.Errorf("isGarminSender(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestParseTestCommand(t *testing.T) {
	t.Run("update with coords", func(t *testing.T) {
		tc := parseTestCommand("update lat:-43.73, long:170.09", "")
		if !tc.IsUpdate || !tc.HasCoords {
			t.Fatalf("want IsUpdate && HasCoords, got %+v", tc)
		}
		if tc.Lat != -43.73 || tc.Lon != 170.09 {
			t.Errorf("coords = (%v, %v), want (-43.73, 170.09)", tc.Lat, tc.Lon)
		}
	})

	t.Run("bare update uses no coords", func(t *testing.T) {
		tc := parseTestCommand("", "UPDATE\r\n")
		if !tc.IsUpdate {
			t.Fatalf("want IsUpdate, got %+v", tc)
		}
		if tc.HasCoords {
			t.Errorf("bare UPDATE should not carry coords, got %+v", tc)
		}
	})

	t.Run("all command", func(t *testing.T) {
		tc := parseTestCommand("all", "")
		if !tc.IsAll {
			t.Fatalf("want IsAll, got %+v", tc)
		}
	})

	t.Run("noise is not a command", func(t *testing.T) {
		tc := parseTestCommand("Re: your update is ready", "thanks, please update me later")
		if tc.IsAll || tc.IsUpdate {
			t.Errorf("substring 'update' inside prose must not trigger a command: %+v", tc)
		}
	})
}

func TestParseGarminBody(t *testing.T) {
	t.Run("inReach UPDATE email (command first, then boilerplate)", func(t *testing.T) {
		body := "UPDATE\r\n\r\n" +
			"View the location or send a reply to Romain:\r\n" +
			"https://inreachlink.com/AB12CD\r\n\r\n" +
			"Romain sent this message from: Lat -43.730000 Lon 170.090000\r\n"
		gc := parseGarminBody(body)
		if gc.Shortlink != "https://inreachlink.com/AB12CD" {
			t.Errorf("shortlink = %q", gc.Shortlink)
		}
		if !gc.HasCoords || gc.Lat != -43.73 || gc.Lon != 170.09 {
			t.Errorf("coords = (%v,%v) hasCoords=%v", gc.Lat, gc.Lon, gc.HasCoords)
		}
		if !gc.Update {
			t.Errorf("UPDATE command not detected: %+v", gc)
		}
	})

	t.Run("emulator colon-form coordinates", func(t *testing.T) {
		body := "Message: START\r\nLat:-43.730000 Lon:170.090000\r\n" +
			"Reply: https://explore.garmin.com/TextMessage/TxtMsg?extId=X&guid=Y\r\n"
		gc := parseGarminBody(body)
		if !gc.HasCoords || gc.Lat != -43.73 || gc.Lon != 170.09 {
			t.Errorf("colon-form coords not parsed: (%v,%v) hasCoords=%v", gc.Lat, gc.Lon, gc.HasCoords)
		}
		if !gc.Start {
			t.Errorf("START not detected: %+v", gc)
		}
		// explore.garmin.com is NOT an inreachlink shortlink — must not match.
		if gc.Shortlink != "" {
			t.Errorf("explore.garmin.com URL should not be treated as a shortlink, got %q", gc.Shortlink)
		}
	})

	t.Run("no coords, no link", func(t *testing.T) {
		gc := parseGarminBody("STOP\r\n")
		if gc.HasCoords || gc.Shortlink != "" || !gc.Stop {
			t.Errorf("unexpected parse: %+v", gc)
		}
	})
}

func TestSplitForGarmin(t *testing.T) {
	t.Run("short message stays whole", func(t *testing.T) {
		got := splitForGarmin("YR T:12C")
		if len(got) != 1 {
			t.Fatalf("want 1 chunk, got %d: %v", len(got), got)
		}
	})

	t.Run("long message splits on the D2 boundary", func(t *testing.T) {
		msg := "YR T:12C D1:41mm | MS(AOR) D1 Rain w/ heavyFalls S W1k:75 2k:95 3k:95 padding padding padding padding padding | D2 Shwrs then rain W1k:100 2k:110 3k:120 | AVL:4-HIGH"
		got := splitForGarmin(msg)
		if len(got) != 2 {
			t.Fatalf("want 2 chunks, got %d: %v", len(got), got)
		}
		if !strings.HasPrefix(got[1], "D2 ") {
			t.Errorf("second chunk should start at the D2 marker, got %q", got[1])
		}
	})
}

// TestParseRealGarminUpdate is the golden test: it asserts the parser extracts
// exactly the right command, shortlink, and coordinates from a REAL Garmin
// "Update" email body — the precise scenario that must work end-to-end.
func TestParseRealGarminUpdate(t *testing.T) {
	if !isGarminSender("no.reply.inreach@garmin.com") {
		t.Fatal("real Garmin sender address must be recognised as Garmin")
	}

	gc := parseGarminBody(realGarminUpdateBody)

	if gc.Shortlink != "https://inreachlink.com/g4plcdeFSKxxEMQbrGbVlbQ" {
		t.Errorf("shortlink = %q, want the inreachlink URL", gc.Shortlink)
	}
	if !gc.HasCoords || gc.Lat != -44.988456 || gc.Lon != 168.904967 {
		t.Errorf("coords = (%v, %v) hasCoords=%v, want (-44.988456, 168.904967)", gc.Lat, gc.Lon, gc.HasCoords)
	}
	if !gc.Update {
		t.Errorf("UPDATE command not detected in real email: %+v", gc)
	}
	// The boilerplate must NOT trip false commands.
	if gc.Start || gc.Stop {
		t.Errorf("boilerplate falsely triggered START/STOP: %+v", gc)
	}
}

// TestExtractRealGarminEml runs the full decode path (MIME + quoted-printable)
// on the captured .eml fixture, then parses it — verifying that what the bot
// actually fetches over IMAP is interpreted correctly.
func TestExtractRealGarminEml(t *testing.T) {
	raw, err := os.ReadFile("testdata/garmin_update.eml")
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}

	body := extractEmailBody(raw)
	if !strings.Contains(body, "https://inreachlink.com/g4plcdeFSKxxEMQbrGbVlbQ") {
		t.Errorf("decoded body missing the (possibly soft-wrapped) shortlink:\n%s", body)
	}

	gc := parseGarminBody(body)
	if gc.Shortlink == "" || !gc.HasCoords || !gc.Update {
		t.Errorf("parse of decoded .eml failed: %+v", gc)
	}
}

func TestUserMessageStripsBoilerplate(t *testing.T) {
	um := strings.TrimSpace(userMessage(realGarminUpdateBody))
	if um != "Update" {
		t.Errorf("userMessage = %q, want just the typed command %q", um, "Update")
	}
}

// TestParseShortlinkCoords covers recovering the sender's location from the
// inReach message page (the shortlink target) when the email body has no
// coordinates. JSON shapes are taken from real captured pages.
func TestParseShortlinkCoords(t *testing.T) {
	t.Run("device GPS message has real coords in Locations[]", func(t *testing.T) {
		page := `...,"FirstName":"Christchurch","LastName":"ACR 01","Locations":[{"FalseBreak":null,"Latitude":-44.9884567260742,"Longitude":168.904968261719,"Altitude":1234}]...`
		lat, lon, ok := parseShortlinkCoords(page)
		if !ok {
			t.Fatal("expected coords, got none")
		}
		if lat > -44.98 || lat < -44.99 || lon < 168.90 || lon > 168.91 {
			t.Errorf("coords out of expected range: (%v, %v)", lat, lon)
		}
	})

	t.Run("app message with no GPS reports 0,0 -> no fix", func(t *testing.T) {
		page := `..."Latitude":0,"Longitude":0,...,"Latitude":0,"Longitude":0...`
		if _, _, ok := parseShortlinkCoords(page); ok {
			t.Error("0,0 must be treated as no fix")
		}
	})

	t.Run("default map noise is ignored, real fix wins", func(t *testing.T) {
		// page also contains default map center as 'lat : 42' (different format) and 0,0 owner loc
		page := `lat : 42, lon : -73 ... "Latitude":0,"Longitude":0 ... "Latitude":-43.5310,"Longitude":170.1420 ...`
		lat, lon, ok := parseShortlinkCoords(page)
		if !ok || lat != -43.5310 || lon != 170.1420 {
			t.Errorf("expected (-43.5310,170.1420), got ok=%v (%v,%v)", ok, lat, lon)
		}
	})
}

func TestLooksLikeGarminError(t *testing.T) {
	if !looksLikeGarminError([]byte(`<input name="__RequestVerificationToken" value="abc">`)) {
		t.Error("re-served verification token form should be flagged as a failure page")
	}
	if !looksLikeGarminError([]byte(`<title>Error</title>`)) {
		t.Error("error title page should be flagged")
	}
	if looksLikeGarminError([]byte(`{"status":"ok"}`)) {
		t.Error("a normal success body must not be flagged")
	}
}
