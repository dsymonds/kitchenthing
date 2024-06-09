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
		want []int
	}{
		// Simple cases.
		{[]string{"apple", "banana", "rye bread"}, []int{0, 1, 2}},
		{[]string{"ice cream", "apples", "bananas"}, []int{1, 2, 0}},
		// Double matches.
		{[]string{"apple", "apple2", "apple3"}, []int{0, 1, 2}},
		// Unmatched elements should end up last.
		{[]string{"apples", "wraps", "ice cream"}, []int{0, 2, 1}},
	}
	for _, test := range tests {
		got := r.Arrange(len(test.in), func(i int) string { return test.in[i] })
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("r.Arrange(%q) = %v, want %v", test.in, got, test.want)
		}
	}
}
