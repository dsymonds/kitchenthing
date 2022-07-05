package main

// Todoist integration.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type renderableTask struct {
	Priority int       // 4, 3, 2, 1
	Time     time.Time // to the minute; only set for tasks with times
	Title    string
	HasDesc  bool   // whether there's a description
	Assignee string // may be empty
	Project  string
}

func (rt renderableTask) Compare(o renderableTask) int {
	if rt.Priority != o.Priority {
		return cmp(o.Priority, rt.Priority) // inverse; higher priority first
	}
	if !rt.Time.IsZero() && !o.Time.IsZero() {
		if c := timeCompare(rt.Time, o.Time); c != 0 {
			return c
		}
	} else if !rt.Time.IsZero() {
		return -1
	} else if !o.Time.IsZero() {
		return 1
	}
	if rt.Project != o.Project {
		return strings.Compare(rt.Project, o.Project)
	}
	if rt.Title != o.Title {
		return strings.Compare(rt.Title, o.Title)
	}
	if rt.HasDesc != o.HasDesc {
		if rt.HasDesc {
			return -1
		}
		return 1
	}
	return strings.Compare(rt.Assignee, o.Assignee)
}

func timeCompare(a, b time.Time) int {
	if a.Before(b) {
		return -1
	}
	if a.After(b) {
		return 1
	}
	return 0
}

// See https://developer.todoist.com/sync/v8/ for the reference for types and protocols.

type todoistProject struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Shared bool   `json:"shared"`
}

type todoistCollaborator struct {
	ID int64 `json:"id"`

	FullName string `json:"full_name"`

	// email
}

type todoistTask struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"project_id"`
	Content     string `json:"content"`     // title of task
	Description string `json:"description"` // secondary info
	Priority    int    `json:"priority"`

	Responsible *int64 `json:"responsible_uid"`
	Checked     int    `json:"checked"`
	Due         *due   `json:"due"`
}

type due struct {
	Date string `json:"date"` // YYYY-MM-DD or YYYY-MM-DDTHH:MM:SS or YYYY-MM-DDTHH:MM:SSZ
	// Parsed from Date on sync.
	y       int
	m       time.Month
	d       int
	hasTime bool
	hh, mm  int // only if hasTime
	due     time.Time

	IsRecurring bool `json:"is_recurring"`
}

// when reports when the due date is relative to today.
// This is -1, 0 or 1 for overdue, today and future due dates.
func (dd *due) when() int {
	now := time.Now()
	// Cheapest check based solely on the date.
	y, m, d := now.Date()
	if dd.y != y {
		return cmp(dd.y, y)
	} else if dd.m != m {
		return cmp(int(dd.m), int(m))
	} else if dd.d != d {
		return cmp(dd.d, d)
	}
	// Remaining check is for things due today.
	if dd.due.Before(now) {
		return -1
	}
	return 0
}

func cmp(x, y int) int {
	if x < y {
		return -1
	}
	return 1
}

func (dd *due) update() error {
	if !strings.Contains(dd.Date, "T") {
		// YYYY-MM-DD (full-day date)
		t, err := time.ParseInLocation("2006-01-02", dd.Date, time.Local)
		if err != nil {
			return fmt.Errorf("parsing full-day date %q: %w", dd.Date, err)
		}
		dd.y, dd.m, dd.d = t.Date()
		dd.due = time.Date(dd.y, dd.m, dd.d, 23, 59, 59, 0, time.Local)
		return nil
	}
	// YYYY-MM-DDTHH:MM:SS or YYYY-MM-DDTHH:MM:SSZ
	str, loc := dd.Date, time.Local
	if strings.HasSuffix(str, "Z") {
		str = str[:len(str)-1]
		loc = time.UTC
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", str, loc)
	if err != nil {
		return fmt.Errorf("parsing due date with time %q: %w", dd.Date, err)
	}
	dd.y, dd.m, dd.d = t.Date()
	dd.hasTime = true
	dd.hh, dd.mm, _ = t.Clock()
	dd.due = t
	return nil
}

type TodoistSyncer struct {
	apiToken string

	// State.
	syncToken     string
	projects      map[int64]todoistProject
	collaborators map[int64]todoistCollaborator
	tasks         map[int64]todoistTask // Only incomplete
}

func NewTodoistSyncer(cfg Config) *TodoistSyncer {
	return &TodoistSyncer{
		apiToken:  cfg.TodoistAPIToken,
		syncToken: "*", // this means next sync should get all data
	}
}

func (ts *TodoistSyncer) Sync(ctx context.Context) error {
	var data struct {
		SyncToken     string                `json:"sync_token"`
		FullSync      bool                  `json:"full_sync"`
		Projects      []todoistProject      `json:"projects"`
		Collaborators []todoistCollaborator `json:"collaborators"`
		Items         []todoistTask         `json:"items"`
	}
	err := ts.post(ctx, "/sync/v8/sync", url.Values{
		"sync_token":     []string{ts.syncToken},
		"resource_types": []string{`["projects","items","collaborators"]`},
	}, &data)
	if err != nil {
		return err
	}

	if data.FullSync || ts.projects == nil {
		// Server says this is a full sync, or this is the first sync we've attempted.
		ts.projects = make(map[int64]todoistProject)
		ts.collaborators = make(map[int64]todoistCollaborator)
		ts.tasks = make(map[int64]todoistTask)
	}
	for _, p := range data.Projects {
		// TODO: Handle deletions. This is pretty uncommon.
		ts.projects[p.ID] = p
	}
	for _, c := range data.Collaborators {
		// TODO: Handle deletions. It's uncommon.
		ts.collaborators[c.ID] = c
	}
	for _, item := range data.Items {
		if item.Checked > 0 {
			delete(ts.tasks, item.ID)
		} else {
			if item.Due != nil {
				item.Due.update()
			}
			ts.tasks[item.ID] = item
		}
	}
	ts.syncToken = data.SyncToken
	return nil
}

func (ts *TodoistSyncer) post(ctx context.Context, path string, params url.Values, dst interface{}) error {
	form := strings.NewReader(params.Encode())
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.todoist.com"+path, form)
	if err != nil {
		return fmt.Errorf("constructing HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.apiToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("reading API response body: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("API request returned %s", resp.Status)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("parsing API response: %w", err)
	}
	return nil
}

func (ts *TodoistSyncer) RenderableTasks() []renderableTask {
	var res []renderableTask

	for _, task := range ts.tasks {
		proj := ts.projects[task.ProjectID]
		if !proj.Shared {
			continue
		}
		if task.Due == nil || task.Due.when() > 0 {
			// No due date, or due after today.
			continue
		}
		rt := renderableTask{
			Priority: task.Priority,
			Title:    task.Content,
			HasDesc:  task.Description != "",
			Project:  proj.Name,
		}
		if task.Responsible != nil {
			name := ts.collaborators[*task.Responsible].FullName
			if i := strings.IndexByte(name, ' '); i >= 0 {
				name = name[:i]
			}
			rt.Assignee = name
		}
		if task.Due.hasTime {
			rt.Time = task.Due.due
		}
		res = append(res, rt)
	}

	sort.Slice(res, func(i, j int) bool { return res[i].Compare(res[j]) < 0 })

	return res
}
