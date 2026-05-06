//go:build integration

package main

import (
	"strings"
	"testing"
)

// Exercise MetService + NZAA for every geofence key in PARKS (network required).
func TestAllParksMetServiceAndNZAA(t *testing.T) {
	for key := range PARKS {
		key := key
		t.Run(key, func(t *testing.T) {
			ms := fetchMetService(key)
			if strings.HasPrefix(ms, "MS:") && ms != "MS:Err" {
				// structured error from parser
				t.Fatalf("MetService: %s", ms)
			}
			if strings.Contains(ms, "W1k:??") || strings.Contains(ms, "W2k:??") {
				t.Fatalf("MetService missing low-level wind: %s", ms)
			}
			if strings.Contains(ms, "3k:??") {
				t.Fatalf("MetService missing 3000m wind (expected estimate or API value): %s", ms)
			}

			av := fetchAvalanche(key)
			switch {
			case strings.HasPrefix(av, "AVL:Err"), strings.HasPrefix(av, "AVL:JSON"):
				t.Fatalf("NZAA: %s", av)
			case av == "AVL:??":
				t.Fatalf("NZAA: unparsed %s", av)
			}
			t.Logf("%s | %s", ms, av)
		})
	}
}
