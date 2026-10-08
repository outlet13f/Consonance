package volume

import "testing"

func TestStartSegment(t *testing.T) {
	label := "START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n" +
		"CHECKPOINT LOCATION: 0/3000080\nBACKUP METHOD: streamed\nBACKUP FROM: primary\n" +
		"START TIME: 2026-10-07 03:15:00 UTC\nLABEL: consonance branch dev\nSTART TIMELINE: 1\n"
	if got := startSegment(label); got != "000000010000000000000003" {
		t.Fatalf("startSegment = %q", got)
	}
	if got := startSegment("LABEL: x\n"); got != "" {
		t.Fatalf("startSegment without the line = %q", got)
	}
}
