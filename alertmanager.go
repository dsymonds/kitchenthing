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
	Summary     string
	Description string
}

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
			Summary:     cleanString(ga.Annotations["summary"]),
			Description: cleanString(ga.Annotations["description"]),
		})
	}

	// TODO: We should be capturing some sort of uniqueness key ("fingerprint"?)
	// since the description may change due to it including metric values,
	// and that probably shouldn't trigger a display refresh.

	// Sort the alerts to try to get some vaguely canonical ordering.
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

	Status *struct {
		State *string `json:"state"` // one of "unprocessed", "active", "suppressed"
	} `json:"status"`
}
