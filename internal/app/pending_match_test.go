package app

import (
	"testing"
	"time"

	mediaproc "github.com/se0wtf/go-garmindives/internal/media"
)

func matchDive(t *testing.T, id int64, start, zone string, duration time.Duration) pendingDive {
	t.Helper()
	value, location, tz, ok := pendingDiveStart(start, zone)
	if !ok {
		t.Fatal("invalid test dive")
	}
	return pendingDive{id: id, name: "Dive", start: value, end: value.Add(duration), location: location, timezone: tz}
}
func TestPendingCaptureMatchingWindowsAndTimezones(t *testing.T) {
	dive := matchDive(t, 42, "2026-08-15T10:00:00", "Pacific/Noumea", time.Hour)
	for _, test := range []struct{ local, offset, reason string }{
		{"2026-08-15T09:50:00", "", "matched"},
		{"2026-08-15T11:10:00", "", "matched"},
		{"2026-08-15T09:49:59", "", "no_matching_dive"},
		{"2026-08-15T11:10:00.001", "", "no_matching_dive"},
		{"2026-08-14T23:30:00", "Z", "matched"},
		{"2026-08-15T10:30:00", "Z", "no_matching_dive"},
	} {
		t.Run(test.local+test.offset, func(t *testing.T) {
			candidates, reason := matchPendingCapture(&mediaproc.CaptureTime{LocalTime: test.local, Offset: test.offset, Source: "EXIF"}, []pendingDive{dive})
			if reason != test.reason {
				t.Fatalf("match: %s %+v", reason, candidates)
			}
			if reason == "matched" && (len(candidates) != 1 || candidates[0].TimezoneAssumed != (test.offset == "")) {
				t.Fatal("timezone provenance lost")
			}
		})
	}
	overlap := matchDive(t, 99, "2026-08-15T10:50:00", "Pacific/Noumea", time.Hour)
	if items, reason := matchPendingCapture(&mediaproc.CaptureTime{LocalTime: "2026-08-15T10:45:00"}, []pendingDive{dive, overlap}); reason != "multiple_matches" || len(items) != 2 {
		t.Fatal("overlap was guessed")
	}
	paris := matchDive(t, 7, "2026-08-15T10:00:00", "Europe/Paris", time.Hour)
	if _, reason := matchPendingCapture(&mediaproc.CaptureTime{LocalTime: "2026-08-15T10:30:00"}, []pendingDive{dive, paris}); reason != "multiple_matches" {
		t.Fatal("multiple local timezones were guessed")
	}
	if items, reason := matchPendingCapture(&mediaproc.CaptureTime{LocalTime: "2026-08-15T10:30:00", Offset: "+02:00"}, []pendingDive{dive, paris}); reason != "matched" || items[0].DiveID != 7 {
		t.Fatal("explicit offset was ignored")
	}
	if _, reason := matchPendingCapture(nil, []pendingDive{dive}); reason != "missing_capture_time" {
		t.Fatal("missing capture date was guessed")
	}
}
func TestPendingCaptureMidnightAndDST(t *testing.T) {
	dive := matchDive(t, 42, "2026-12-31T23:55:00", "Pacific/Noumea", 30*time.Minute)
	if _, reason := matchPendingCapture(&mediaproc.CaptureTime{LocalTime: "2027-01-01T00:30:00"}, []pendingDive{dive}); reason != "matched" {
		t.Fatal("cross-midnight dive did not match")
	}
	fold := matchDive(t, 43, "2026-10-25T02:15:00+02:00", "Europe/Paris", 30*time.Minute)
	if items, reason := matchPendingCapture(&mediaproc.CaptureTime{LocalTime: "2026-10-25T02:30:00"}, []pendingDive{fold}); reason != "ambiguous_time" || len(items) != 1 {
		t.Fatalf("DST fold was guessed: %s %+v", reason, items)
	}
	paris, _ := time.LoadLocation("Europe/Paris")
	if values := localClockInstants("2026-03-29T02:30:00", paris); len(values) != 0 {
		t.Fatal("nonexistent local time accepted")
	}
	if _, _, _, ok := pendingDiveStart("2026-10-25T02:30:00", "Europe/Paris"); ok {
		t.Fatal("ambiguous dive start accepted without an offset")
	}
}
