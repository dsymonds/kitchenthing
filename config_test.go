package main

import (
	"testing"
)

func TestConfigParses(t *testing.T) {
	cfg, err := parseConfig(*configFile)
	if err != nil {
		t.Fatalf("Bad config: %v", err)
	}

	// Check that we can construct reorderers.
	for _, o := range cfg.Orderings {
		_, err := NewReorderer(o.Groups)
		if err != nil {
			t.Errorf("Creating Reorderer for project %q based on config: %v", o.Project, err)
		}
	}
}
