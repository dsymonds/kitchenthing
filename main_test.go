package main

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestServerWriteDoesNotSpin(t *testing.T) {
	// TODO: Use t.SetTimeout when it exists.
	// https://github.com/golang/go/issues/48157

	// There was a bug where if the buffer started with \n
	// and it needed shrinking then it would spin in Write
	// while holding s.mu, and never escape.
	s := &server{}
	io.WriteString(&s.logBuf, "\nsomething to trim\n")
	for i := 0; i < 200; i++ {
		io.WriteString(&s.logBuf, strings.Repeat("x", 1<<10)+"\n")
	}
	// This would break:
	io.WriteString(s, "the final straw")
}

func TestFormatTime(t *testing.T) {
	tests := []struct {
		t time.Time
		s string
	}{
		// Midnight or soon after:
		{time.Date(2025, 12, 28, 0, 0, 0, 0, time.UTC), "12AM"},
		{time.Date(2025, 12, 28, 0, 15, 0, 0, time.UTC), "12:15AM"},
		// Noon, or just before/after:
		{time.Date(2025, 12, 28, 11, 50, 0, 0, time.UTC), "11:50AM"},
		{time.Date(2025, 12, 28, 12, 0, 0, 0, time.UTC), "12PM"},
		{time.Date(2025, 12, 28, 12, 30, 0, 0, time.UTC), "12:30PM"},
		// Afternoon/evening:
		{time.Date(2025, 12, 28, 17, 0, 0, 0, time.UTC), "5PM"},
		{time.Date(2025, 12, 28, 19, 30, 0, 0, time.UTC), "7:30PM"},
	}
	for _, test := range tests {
		got := FormatTime(test.t)
		if got != test.s {
			t.Errorf("FormatTime(%v) = %q, want %q", test.t, got, test.s)
		}
	}
}
