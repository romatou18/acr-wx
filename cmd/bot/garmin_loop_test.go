//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"net/smtp"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// Fake session tokens — structurally valid but rejected by Garmin's endpoint.
// GARMIN_DRY_RUN=1 bypasses that endpoint entirely.
const (
	emulatorExtID = "EMULATOR_EXT_001"
	emulatorGUID  = "emu1ator-0000-0000-0000-test00000001"
	emulatorLat   = -43.730000 // Aoraki / Mt Cook
	emulatorLon   = 170.090000
)

// TestGarminDeviceLoop exercises the full Garmin message path without a real
// device:
//
//  1. Sends a crafted inReach-format email (Option B emulator).
//  2. Waits for it to appear in the INBOX, then runs handler() with
//     GARMIN_DRY_RUN=1, which routes the weather report back to EMAIL_USER
//     by email instead of POSTing to Garmin (Option C).
//  3. Polls the inbox for the dry-run reply and asserts the report structure.
func TestGarminDeviceLoop(t *testing.T) {
	emailUser := os.Getenv("EMAIL_USER")
	emailPass := os.Getenv("EMAIL_PASS")
	if emailUser == "" || emailPass == "" {
		t.Skip("EMAIL_USER / EMAIL_PASS not set — skipping Garmin loop test")
	}
	if os.Getenv("TURSO_DB_URL") == "" {
		t.Skip("TURSO_DB_URL not set — skipping Garmin loop test")
	}

	// Enable dry-run so sendToGarmin emails the report instead of hitting Garmin.
	t.Setenv("GARMIN_DRY_RUN", "1")
	t.Setenv("GARMIN_DRY_RUN_REPLY_TO", emailUser)

	// ── Step 1: send the Garmin emulator email ────────────────────────────────
	t.Log("Step 1: sending Garmin emulator email…")
	if err := sendGarminEmulatorEmail(emailUser, emailPass, emailUser); err != nil {
		t.Fatalf("failed to send emulator email: %v", err)
	}
	t.Log("  ✅ emulator email sent")

	// ── Step 2: wait for the email to appear in INBOX ─────────────────────────
	t.Log("Step 2: waiting for emulator email to land in INBOX…")
	if err := waitForEmail(emailUser, emailPass, "inReach message from", 60*time.Second); err != nil {
		t.Fatalf("emulator email never arrived: %v", err)
	}
	t.Log("  ✅ emulator email visible in INBOX")

	// ── Step 3: run the bot handler ───────────────────────────────────────────
	t.Log("Step 3: running bot handler…")
	if err := handler(context.Background()); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	t.Log("  ✅ handler completed")

	// ── Step 4: poll inbox for the dry-run reply ──────────────────────────────
	t.Log("Step 4: polling inbox for dry-run reply…")
	body, err := findDryRunReply(emailUser, emailPass, 60*time.Second)
	if err != nil {
		t.Fatalf("inbox poll failed: %v", err)
	}
	if body == "" {
		t.Fatal("no dry-run reply found in inbox after timeout")
	}
	t.Logf("  ✅ dry-run reply received:\n%s", body)

	// ── Step 5: assert report structure ──────────────────────────────────────
	for _, want := range []string{"YR T:", "MS(", "W1k:", "AVL:"} {
		if !strings.Contains(body, want) {
			t.Errorf("reply missing expected field %q\nfull body:\n%s", want, body)
		}
	}
}

// sendGarminEmulatorEmail crafts an email that mimics what a Garmin inReach
// device sends after a location ping — including the reply URL that carries
// the extId/guid session tokens and the inline Lat/Lon stamp.
func sendGarminEmulatorEmail(fromAddr, pass, toAddr string) error {
	// Mirrors a REAL inReach "Update" email (single-part text/plain, shortlink +
	// "Lat .. Lon .." stamp + boilerplate), captured from a live device.
	//
	// NOTE: Gmail SMTP rewrites From to the authenticated account, so this email
	// does NOT arrive from a garmin.com address — isGarminSender() is false. To
	// still drive the Garmin reply path (not the human "test reply" path), the
	// device message is phrased inline ("UPDATE wx") so it does not match the
	// bare-^update$ test trigger and instead falls through to the Garmin parser,
	// which keys off the inreachlink shortlink. Combined with GARMIN_DRY_RUN=1 the
	// bot emails back a "Garmin Dry Run" reply instead of POSTing to Garmin. Swap
	// in a real, fresh shortlink to also exercise the live session/anti-bot
	// handshake in InitGarminSession.
	body := fmt.Sprintf(
		"UPDATE wx\r\n"+
			"\r\n"+
			"View the location or send a reply to Emulator Device:\r\n"+
			"https://inreachlink.com/EMULATORtestlink\r\n"+
			"\r\n"+
			"Emulator Device sent this message from: Lat %.6f Lon %.6f\r\n"+
			"\r\n"+
			"Do not reply directly to this message.\r\n",
		emulatorLat, emulatorLon,
	)

	raw := []byte(
		"From: " + fromAddr + "\r\n" +
			"To: " + toAddr + "\r\n" +
			"Subject: inReach message from Emulator Device\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=UTF-8\r\n" +
			"\r\n" +
			body,
	)

	auth := smtp.PlainAuth("", fromAddr, pass, "smtp.gmail.com")
	return smtp.SendMail("smtp.gmail.com:587", auth, fromAddr, []string{toAddr}, raw)
}

// waitForEmail polls until an unseen message whose subject contains substrMatch
// appears in INBOX, or until the timeout expires.
func waitForEmail(emailUser, emailPass, substrMatch string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		found, err := imapSearchUnseen(emailUser, emailPass, substrMatch)
		if err != nil {
			return err
		}
		if found {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("timed out after %s waiting for email with subject containing %q", timeout, substrMatch)
}

// findDryRunReply polls the INBOX for an unseen email whose subject contains
// "Garmin Dry Run", reads the body, marks it seen, and returns the body text.
func findDryRunReply(emailUser, emailPass string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body, err := imapFetchBody(emailUser, emailPass, "Garmin Dry Run")
		if err != nil {
			return "", err
		}
		if body != "" {
			return body, nil
		}
		time.Sleep(3 * time.Second)
	}
	return "", nil
}

// imapSearchUnseen returns true if any unseen message subject contains needle.
func imapSearchUnseen(emailUser, emailPass, needle string) (bool, error) {
	c, err := imapLogin(emailUser, emailPass)
	if err != nil {
		return false, err
	}
	defer c.Logout() //nolint:errcheck

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	seqNums, err := c.Search(criteria)
	if err != nil {
		return false, fmt.Errorf("IMAP search: %w", err)
	}
	if len(seqNums) == 0 {
		return false, nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(seqNums...)
	msgs := make(chan *imap.Message, len(seqNums))
	if err := c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope}, msgs); err != nil {
		return false, fmt.Errorf("IMAP fetch envelopes: %w", err)
	}
	for msg := range msgs {
		if strings.Contains(msg.Envelope.Subject, needle) {
			return true, nil
		}
	}
	return false, nil
}

// imapFetchBody fetches the body of the first unseen message whose subject
// contains needle, marks it seen, and returns the body as a string.
func imapFetchBody(emailUser, emailPass, needle string) (string, error) {
	c, err := imapLogin(emailUser, emailPass)
	if err != nil {
		return "", err
	}
	defer c.Logout() //nolint:errcheck

	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{imap.SeenFlag}
	seqNums, err := c.Search(criteria)
	if err != nil {
		return "", fmt.Errorf("IMAP search: %w", err)
	}
	if len(seqNums) == 0 {
		return "", nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(seqNums...)
	section := &imap.BodySectionName{}
	msgs := make(chan *imap.Message, len(seqNums))
	if err := c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, section.FetchItem()}, msgs); err != nil {
		return "", fmt.Errorf("IMAP fetch: %w", err)
	}

	for msg := range msgs {
		if !strings.Contains(msg.Envelope.Subject, needle) {
			continue
		}
		r := msg.GetBody(section)
		if r == nil {
			continue
		}
		raw, err := io.ReadAll(r)
		if err != nil {
			return "", fmt.Errorf("reading body: %w", err)
		}

		single := new(imap.SeqSet)
		single.AddNum(msg.SeqNum)
		_ = c.Store(single, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil)

		return string(raw), nil
	}
	return "", nil
}

func imapLogin(emailUser, emailPass string) (*client.Client, error) {
	c, err := client.DialTLS("imap.gmail.com:993", nil)
	if err != nil {
		return nil, fmt.Errorf("IMAP connect: %w", err)
	}
	if err := c.Login(emailUser, emailPass); err != nil {
		c.Logout() //nolint:errcheck
		return nil, fmt.Errorf("IMAP login: %w", err)
	}
	if _, err := c.Select("INBOX", false); err != nil {
		c.Logout() //nolint:errcheck
		return nil, fmt.Errorf("IMAP select: %w", err)
	}
	return c, nil
}
