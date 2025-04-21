package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
)

func FetchHASS(ctx context.Context, addr, token, template string) (string, error) {
	// https://developers.home-assistant.io/docs/api/rest/

	treq := struct {
		Template string `json:"template"`
	}{Template: template}
	body, err := json.Marshal(treq)
	if err != nil {
		return "", fmt.Errorf("marshaling JSON body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/api/template", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("making HTTP request: %w", err)
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("reading HTTP response body: %w", err)
	}
	if resp.StatusCode != 200 {
		log.Printf("HASS error %s: [%s]", resp.Status, respBody)
		return "", fmt.Errorf("non-200 response: %s", resp.Status)
	}
	return string(respBody), nil
}
