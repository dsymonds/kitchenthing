package main

import (
	"reflect"
	"testing"
)

func TestReorder(t *testing.T) {
	patterns := []string{
		"apple.*",
		"banana.*",
		".*bread.*",
		".*cream.*",
	}
	t.Logf("Input patterns: %q", patterns)
	r, err := NewReorderer(patterns)
	if err != nil {
		t.Fatalf("NewReorderer: %v", err)
	}
	tests := []struct {
		in   []string
		want Arrangement
	}{
		// Simple cases.
		{[]string{"apple", "banana", "rye bread"}, Arrangement{New: []int{0, 1, 2}, NumUnknown: 0}},
		{[]string{"ice cream", "apples", "bananas"}, Arrangement{New: []int{1, 2, 0}, NumUnknown: 0}},
		// Double matches.
		{[]string{"apple", "apple2", "apple3"}, Arrangement{New: []int{0, 1, 2}, NumUnknown: 0}},
		// Unmatched elements should end up last.
		{[]string{"pavlova", "apples", "wraps", "ice cream"}, Arrangement{New: []int{1, 3, 0, 2}, NumUnknown: 2}},
	}
	for _, test := range tests {
		got := r.Arrange(len(test.in), func(i int) string { return test.in[i] })
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("r.Arrange(%q) = %v, want %v", test.in, got, test.want)
		}
	}
}
