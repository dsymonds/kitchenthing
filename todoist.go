package main

// Todoist integration.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

type renderableTask struct {
	Priority int // 4, 3, 2, 1
	Title    string
	Assignee string // may be empty
}

type todoistProject struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Shared bool   `json:"shared"`
}

type todoistCollaborator struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// email
}

type todoistTask struct {
	ProjectID int64  `json:"project_id"`
	Content   string `json:"content"`
	Priority  int    `json:"priority"`
	Assignee  *int64 `json:"assignee"`
	// completed
}

func TodoistTasks(ctx context.Context, cfg Config) ([]renderableTask, error) {
	sharedProjects := make(map[int64]bool)
	collaborators := make(map[int64]todoistCollaborator)

	// TODO: fetching projects/collaborators probably only need to happen hourly/daily

	var projects []todoistProject
	if err := todoistGET(ctx, cfg, "/rest/v1/projects", &projects); err != nil {
		return nil, fmt.Errorf("getting projects: %v", err)
	}
	for _, proj := range projects {
		if !proj.Shared {
			continue
		}
		sharedProjects[proj.ID] = true

		// TODO: do these in parallel
		var collabs []todoistCollaborator
		if err := todoistGET(ctx, cfg, fmt.Sprintf("/rest/v1/projects/%d/collaborators", proj.ID), &collabs); err != nil {
			return nil, fmt.Errorf("getting collaborators for project %q: %v", proj.Name, err)
		}
		for _, collab := range collabs {
			collaborators[collab.ID] = collab
		}
	}

	var res []renderableTask

	var tasks []todoistTask
	if err := todoistGET(ctx, cfg, "/rest/v1/tasks?filter=(today|overdue)", &tasks); err != nil {
		return nil, fmt.Errorf("getting tasks: %v", err)
	}
	for _, task := range tasks {
		if !sharedProjects[task.ProjectID] {
			continue
		}
		rt := renderableTask{
			Priority: task.Priority,
			Title:    task.Content,
		}
		if task.Assignee != nil {
			name := collaborators[*task.Assignee].Name
			if i := strings.IndexByte(name, ' '); i >= 0 {
				name = name[:i]
			}
			rt.Assignee = name
		}
		res = append(res, rt)
	}

	sort.Slice(res, func(i, j int) bool { return res[i].Priority > res[j].Priority })

	return res, nil
}

func todoistGET(ctx context.Context, cfg Config, path string, dst interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.todoist.com"+path, nil)
	if err != nil {
		return fmt.Errorf("constructing HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.TodoistAPIToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("performing HTTP request: %w", err)
	} else if resp.StatusCode != 200 {
		return fmt.Errorf("API request returned %s", resp.Status)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("parsing API response: %w", err)
	}
	return nil
}
