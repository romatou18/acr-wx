// cmd/test-email/main.go
package main

import (
	"fmt"
	"log"
	"os"
        "github.com/emersion/go-imap/client"
)

func main() {
	user := os.Getenv("EMAIL_USER")
	pass := os.Getenv("EMAIL_PASS")

	if user == "" || pass == "" {
		log.Fatal("❌ EMAIL_USER or EMAIL_PASS is not set in the environment.")
	}

	fmt.Printf("🔍 Attempting to connect to imap.gmail.com:993 for %s...\n", user)

	// Connect to Gmail IMAP server
	c, err := client.DialTLS("imap.gmail.com:993", nil)
	if err != nil {
		log.Fatalf("❌ Failed to connect to IMAP server: %v", err)
	}
	defer c.Logout()
	fmt.Println("✅ Connected to Gmail IMAP server.")

	// Attempt Login
	if err := c.Login(user, pass); err != nil {
		log.Fatalf("❌ Authentication failed. Check your App Password: %v", err)
	}
	fmt.Println("✅ Authentication successful!")

	// Select INBOX to verify read permissions
	mbox, err := c.Select("INBOX", false)
	if err != nil {
		log.Fatalf("❌ Failed to select INBOX: %v", err)
	}
	fmt.Printf("✅ INBOX selected successfully. You have %d total messages.\n", mbox.Messages)
	fmt.Println("🎉 Email setup is fully working and ready for Netlify!")
}
