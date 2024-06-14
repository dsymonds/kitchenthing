package main

import (
	"fmt"
	"regexp"
	"sort"
)

type GroupPatterns struct {
	Name     string   `yaml:"name"`
	Patterns []string `yaml:"patterns"`
}

type Reorderer struct {
	patterns []match
}

type match struct {
	rx    *regexp.Regexp
	group string
}

func NewReorderer(groups []GroupPatterns) (*Reorderer, error) {
	r := &Reorderer{}
	for _, gp := range groups {
		for _, pat := range gp.Patterns {
			// Make patterns case insensitive by default,
			// and anchor the match.
			pat = "(?i)^" + pat + "$"

			rx, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("bad pattern /%s/: %w", pat, err)
			}
			r.patterns = append(r.patterns, match{rx: rx, group: gp.Name})
		}
	}
	return r, nil
}

type Arrangement struct {
	// New is the new ordering of the indexes provided to Arrange.
	New []int
	// Groups is the groups that each element belongs to.
	// When this is shorter than New, the tail end of that slice
	// are the elements that did not match any of the reorderer's patterns.
	Groups []string
}

// Arrange reorders a slice of the given length, with text retrieved using the given function.
// It returns an ordered list of the original indexes.
func (r *Reorderer) Arrange(n int, text func(int) string) Arrangement {
	// Transform inputs into indexes into r.patterns.
	// Take the first match, and record -1 as a non-match.
	type indexPair struct {
		orig int // the original index
		pati int // index into the r.patterns slice
	}
	var pairs []indexPair
	for i := 0; i < n; i++ {
		s := text(i)
		pati := -1
		for j, m := range r.patterns {
			if m.rx.MatchString(s) {
				pati = j
				break
			}
		}
		pairs = append(pairs, indexPair{orig: i, pati: pati})
	}

	// Sort the indexes, using the patis slice to decide the ordering.
	sort.SliceStable(pairs, func(i, j int) (out bool) {
		// Push matched items first, then order based on which pattern they matched.
		if pi, pj := pairs[i].pati, pairs[j].pati; pi >= 0 && pj >= 0 {
			return pi < pj
		} else if pi >= 0 && pj < 0 {
			return true
		} else if pi < 0 && pj >= 0 {
			return false
		}
		// Neither matched a pattern, so compare only on their original index.
		return pairs[i].orig < pairs[j].orig
	})

	var arr Arrangement
	for _, p := range pairs {
		arr.New = append(arr.New, p.orig)
		if p.pati >= 0 {
			arr.Groups = append(arr.Groups, r.patterns[p.pati].group)
		}
	}
	return arr
}
