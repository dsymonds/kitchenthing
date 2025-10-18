package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
)

type HASS struct {
	addr  string
	token string
}

func (h *HASS) RenderTemplate(ctx context.Context, template string) (string, error) {
	// https://developers.home-assistant.io/docs/api/rest/

	treq := struct {
		Template string `json:"template"`
	}{Template: template}
	body, err := h.post(ctx, "/api/template", treq)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (h *HASS) FireEvent(ctx context.Context, eventType string, eventData any) error {
	body, err := h.post(ctx, "/api/events/"+url.PathEscape(eventType), eventData)
	if err != nil {
		return err
	}
	var resp struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decoding JSON response: %w", err)
	}
	log.Printf("HASS event %s fired; HASS responded with %q", eventType, resp.Message)
	return nil
}

func (h *HASS) post(ctx context.Context, path string, treq any) ([]byte, error) {
	body, err := json.Marshal(treq)
	if err != nil {
		return nil, fmt.Errorf("marshaling JSON body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+h.addr+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("making HTTP request: %w", err)
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading HTTP response body: %w", err)
	}
	if resp.StatusCode != 200 {
		log.Printf("HASS error %s: [%s]", resp.Status, respBody)
		return nil, fmt.Errorf("non-200 response: %s", resp.Status)
	}
	return respBody, nil
}
