//go:build integration

package main

import (
	"strings"
	"testing"

	"acr-wx/internal/forecast"
)

// Exercise MetService + NZAA for every geofence key in Parks (network required).
func TestAllParksMetServiceAndNZAA(t *testing.T) {
	for key := range forecast.Parks {
		key := key
		t.Run(key, func(t *testing.T) {
			ms := forecast.FetchMetService(key)
			if strings.HasPrefix(ms, "MS:") && ms != "MS:Err" {
				t.Fatalf("MetService: %s", ms)
			}
			// Only fail if we have 2k wind data but the 3k estimate is still ??
			// (some parks don't provide winds at all altitude levels — that's fine).
			if !strings.Contains(ms, "2k:??") && strings.Contains(ms, "3k:??") {
				t.Fatalf("MetService has 2k wind but missing 3k estimate: %s", ms)
			}

			av := forecast.FetchAvalanche(key)
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
