package main

import (
	"reflect"
	"testing"
)

func TestReorder(t *testing.T) {
	groups := []GroupPatterns{
		{Name: "fresh", Patterns: []string{"apple.*", "banana.*"}},
		{Name: "bread", Patterns: []string{".*bread.*"}},
		{Name: "cold", Patterns: []string{".*cream.*"}},
	}
	t.Logf("Input groups: %q", groups)
	r, err := NewReorderer(groups)
	if err != nil {
		t.Fatalf("NewReorderer: %v", err)
	}
	tests := []struct {
		in   []string
		want Arrangement
	}{
		// Simple cases.
		{[]string{"apple", "banana", "rye bread"}, Arrangement{New: []int{0, 1, 2}, Groups: []string{"fresh", "fresh", "bread"}}},
		{[]string{"ice cream", "apples", "bananas"}, Arrangement{New: []int{1, 2, 0}, Groups: []string{"fresh", "fresh", "cold"}}},
		// Double matches.
		{[]string{"apple", "apple2", "apple3"}, Arrangement{New: []int{0, 1, 2}, Groups: []string{"fresh", "fresh", "fresh"}}},
		// Unmatched elements should end up last.
		{[]string{"pavlova", "apples", "wraps", "ice cream"}, Arrangement{New: []int{1, 3, 0, 2}, Groups: []string{"fresh", "cold"}}},
	}
	for _, test := range tests {
		got := r.Arrange(len(test.in), func(i int) string { return test.in[i] })
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("r.Arrange(%q) = %v, want %v", test.in, got, test.want)
		}
	}
}
