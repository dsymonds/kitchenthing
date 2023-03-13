package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"strings"
)

// Alertmanager integration

type Alert struct {
	Fingerprint string // The uniqueness key for the alert.

	Summary     string
	Description string
}

// Same reports whether the alert is the same as some other alert.
// This works off the alert fingerprint instead of its annotations.
func (a Alert) Same(other Alert) bool { return a.Fingerprint == other.Fingerprint }

func FetchAlerts(ctx context.Context, amAddr string) ([]Alert, error) {
	u := "http://" + amAddr + "/api/v2/alerts" // This gets all active alerts, even silenced/inhibited ones.

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("internal error: constructing http request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}
	raw, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading HTTP response body: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("non-200 response: %s", resp.Status)
	}

	var gas gettableAlerts
	if err := json.Unmarshal(raw, &gas); err != nil {
		return nil, fmt.Errorf("decoding JSON: %w", err)
	}

	var alerts []Alert
	for _, ga := range gas {
		alerts = append(alerts, Alert{
			Fingerprint: ga.Fingerprint,
			Summary:     cleanString(ga.Annotations["summary"]),
			Description: cleanString(ga.Annotations["description"]),
		})
	}

	// Sort the alerts to try to get some vaguely canonical ordering.
	// Alertmanager itself sorts by the fingerprint, which isn't useful for us.
	sort.Slice(alerts, func(i, j int) bool {
		ai, aj := alerts[i], alerts[j]
		if ai.Summary != aj.Summary {
			return ai.Summary < aj.Summary
		}
		return ai.Description < aj.Description
	})

	return alerts, nil
}

func cleanString(s string) string {
	s = strings.TrimSpace(s)

	// Our chosen font doesn't support a lot of glyphs.
	s = strings.Replace(s, "℃", "°C", -1)

	return s
}

// This is a subset of github.com/prometheus/alertmanager/api/v2/models.GettableAlerts,
// but without the huge pile of dependencies.

type gettableAlerts []*gettableAlert

type gettableAlert struct {
	Annotations map[string]string `json:"annotations"`
	Fingerprint string            `json:"fingerprint"`

	Status *struct {
		State *string `json:"state"` // one of "unprocessed", "active", "suppressed"
	} `json:"status"`
}
