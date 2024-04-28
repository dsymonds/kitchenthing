package main

// Todoist integration.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/dsymonds/todoist"
)

type renderableTask struct {
	Priority int       // 4, 3, 2, 1
	Time     time.Time // to the minute; only set for tasks with times
	Title    string
	HasDesc  bool   // whether there's a description
	Assignee string // may be empty
	Project  string

	// Progress:
	Done, Total int
	InProgress  bool // the in-progress label
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
	if rt.Total != o.Total {
		return cmp(rt.Total, o.Total)
	}
	if rt.Done != o.Done {
		return cmp(rt.Done, o.Done)
	}
	if rt.InProgress != o.InProgress {
		if rt.InProgress {
			return -1
		}
		return 1
	}
	return strings.Compare(rt.Assignee, o.Assignee)
}

func cmp(x, y int) int {
	if x < y {
		return -1
	}
	return 1
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

func RenderableTasks(ts *todoist.Syncer) []renderableTask {
	var res []renderableTask

	for _, task := range ts.Items {
		proj := ts.Projects[task.ProjectID]
		if !proj.Shared {
			continue
		}
		if task.Due == nil || task.Due.When() > 0 {
			// No due date, or due after today.
			continue
		}
		rt := renderableTask{
			Priority: task.Priority,
			Title:    task.Content,
			HasDesc:  task.Description != "",
			Project:  proj.Name,

			Done:  task.ChildCompleted,
			Total: task.ChildCompleted + task.ChildRemaining,
		}
		if task.Responsible != nil {
			name := ts.Collaborators[*task.Responsible].FullName
			if i := strings.IndexByte(name, ' '); i >= 0 {
				name = name[:i]
			}
			rt.Assignee = name
		}
		if t, ok := task.Due.Time(); ok {
			rt.Time = t
		}
		for _, label := range task.Labels {
			if label == "in-progress" {
				rt.InProgress = true
			}
		}
		res = append(res, rt)
	}

	sort.Slice(res, func(i, j int) bool { return res[i].Compare(res[j]) < 0 })

	return res
}

func ApplyMetadata(ctx context.Context, ts *todoist.Syncer, mutate bool) {
	for _, item := range ts.Items {
		for _, label := range item.Labels {
			if strings.HasPrefix(label, "m:") {
				if err := applyMetadata(ctx, ts, item, label, mutate); err != nil {
					log.Printf("Applying metadata label %q to item %s (%q): %v", label, item.ID, item.Content, err)
				}
			}
		}
	}
}

func applyMetadata(ctx context.Context, ts *todoist.Syncer, item todoist.Item, label string, mutate bool) error {
	switch label {
	case "m:uf":
		// Unassign if the item is due in the future (after today).
		if item.Due.When() <= 0 {
			return nil
		}
		if item.Responsible != nil {
			if !mutate {
				log.Printf("Would unassign %s (%q)...", item.ID, item.Content)
			} else {
				if err := ts.Assign(ctx, item, ""); err != nil {
					return fmt.Errorf("unassigning: %w", err)
				}
				log.Printf("Unassigned %q", item.Content)
			}
		}

		// Remove any "in-progress" label.
		var labels []string
		for _, label := range item.Labels {
			if label != "in-progress" {
				labels = append(labels, label)
			}
		}
		if len(labels) != len(item.Labels) {
			if !mutate {
				log.Printf("Would change label set from %v to %v", item.Labels, labels)
			} else {
				err := ts.UpdateItem(ctx, item, todoist.ItemUpdates{Labels: &labels})
				if err != nil {
					return fmt.Errorf("removing labels: %w", err)
				}
				log.Printf("Changed label set from %v to %v", item.Labels, labels)
			}
		}

		return nil
	case "m:dd":
		// If there's any other tasks with the same title, and a lower ID,
		// complete this task automatically.
		matched := false
		for _, other := range ts.Items {
			if other.Content == item.Content && other.ID < item.ID {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
		if !mutate {
			log.Printf("Would delete %s (%q)...", item.ID, item.Content)
			return nil
		}
		if err := ts.DeleteItem(ctx, item); err != nil {
			return fmt.Errorf("deleting item: %w", err)
		}
		log.Printf("Deleted duplicate item %s (%q)...", item.ID, item.Content)
	}

	return nil
}
