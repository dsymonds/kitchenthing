package main

import (
	"io"
	"strings"
	"testing"
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
