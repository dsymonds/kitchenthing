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
	HasDesc  bool // whether there's a description
	Overdue  bool
	Assignee string // may be empty
	Project  string

	// Progress:
	Done, Total int
	InProgress  bool // the in-progress label
	PowerHungry bool // the power-hungry label
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
	if rt.Overdue != o.Overdue {
		return boolCompare(rt.Overdue, o.Overdue)
	}
	if rt.Total != o.Total {
		return cmp(rt.Total, o.Total)
	}
	if rt.Done != o.Done {
		return cmp(rt.Done, o.Done)
	}
	if rt.InProgress != o.InProgress {
		return boolCompare(rt.InProgress, o.InProgress)
	}
	if rt.PowerHungry != o.PowerHungry {
		return boolCompare(rt.PowerHungry, o.PowerHungry)
	}
	return strings.Compare(rt.Assignee, o.Assignee)
}

func cmp(x, y int) int {
	if x < y {
		return -1
	}
	return 1
}

func boolCompare(a, b bool) int { // sorts true first
	if a && !b {
		return -1
	} else if !a && b {
		return 1
	}
	return 0
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

	for _, task := range ts.Tasks {
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
			Overdue:  task.Due.When() < 0,
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
			t = t.Local() // Force it to be local, even though tasks usually should already be that.
			rt.Time = t
		}
		for _, label := range task.Labels {
			switch label {
			case "in-progress":
				rt.InProgress = true
			case "power-hungry":
				rt.PowerHungry = true
			}
		}
		res = append(res, rt)
	}

	sort.Slice(res, func(i, j int) bool { return res[i].Compare(res[j]) < 0 })

	return res
}

func ApplyMetadata(ctx context.Context, ts *todoist.Syncer, mutate bool) {
	for _, task := range ts.Tasks {
		for _, label := range task.Labels {
			if strings.HasPrefix(label, "m:") {
				if err := applyMetadata(ctx, ts, task, label, mutate); err != nil {
					log.Printf("Applying metadata label %q to task %s (%q): %v", label, task.ID, task.Content, err)
				}
			}
		}
	}
}

func applyMetadata(ctx context.Context, ts *todoist.Syncer, task todoist.Task, label string, mutate bool) error {
	switch label {
	case "m:uf":
		// Unassign if the task is due in the future (after today).
		if task.Due == nil || task.Due.When() <= 0 {
			return nil
		}
		if task.Responsible != nil {
			if !mutate {
				log.Printf("Would unassign %s (%q)...", task.ID, task.Content)
			} else {
				if err := ts.Assign(ctx, task.ID, ""); err != nil {
					return fmt.Errorf("unassigning: %w", err)
				}
				log.Printf("Unassigned %q", task.Content)
			}
		}

		// Remove any "in-progress" label.
		var labels []string
		for _, label := range task.Labels {
			if label != "in-progress" {
				labels = append(labels, label)
			}
		}
		if len(labels) != len(task.Labels) {
			if !mutate {
				log.Printf("Would change label set from %v to %v", task.Labels, labels)
			} else {
				err := ts.UpdateTask(ctx, task.ID, todoist.TaskUpdates{Labels: &labels})
				if err != nil {
					return fmt.Errorf("removing labels: %w", err)
				}
				log.Printf("Changed label set from %v to %v", task.Labels, labels)
			}
		}

		return nil
	case "m:dd":
		// If there's any other tasks with the same title in the same project, and a lower ID,
		// complete this task automatically.
		matched := false
		for _, other := range ts.Tasks {
			if other.Content == task.Content && other.ProjectID == task.ProjectID && other.ID < task.ID {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
		if !mutate {
			log.Printf("Would delete %s (%q)...", task.ID, task.Content)
			return nil
		}
		if err := ts.DeleteTask(ctx, task.ID); err != nil {
			return fmt.Errorf("deleting task: %w", err)
		}
		log.Printf("Deleted duplicate task %s (%q)...", task.ID, task.Content)
	case "m:rem":
		// Add at-time and -1h reminders for the user this task is assigned to.
		if task.Responsible == nil {
			return nil
		}
		ip := func(i int) *int { return &i }
		want := []todoist.Reminder{
			// TODO: Make this set configurable somehow.
			{TaskID: task.ID, UserID: *task.Responsible, Type: "relative", MinuteOffset: ip(0)},
			{TaskID: task.ID, UserID: *task.Responsible, Type: "relative", MinuteOffset: ip(60)},
		}
		// Remove from want any reminders we already have.
		for _, rem := range ts.Reminders {
			for i := len(want) - 1; i >= 0; i-- {
				if equivReminders(rem, want[i]) {
					copy(want[i:], want[i+1:])
					want = want[:len(want)-1]
				}
			}
		}
		if len(want) == 0 {
			return nil
		}
		if !mutate {
			log.Printf("Would add %d reminders to task %q", len(want), task.Content)
			return nil
		}
		for _, rem := range want {
			if err := ts.AddReminder(ctx, rem); err != nil {
				return fmt.Errorf("adding reminder: %w", err)
			}
			log.Printf("Added reminder to %q", task.Content)
		}
	}

	return nil
}

func equivReminders(a, b todoist.Reminder) bool {
	if a.TaskID != b.TaskID || a.UserID != b.UserID || a.Type != b.Type {
		return false
	}
	if (a.MinuteOffset != nil) != (b.MinuteOffset != nil) {
		return false
	}
	if a.MinuteOffset != nil && (*a.MinuteOffset != *b.MinuteOffset) {
		return false
	}
	return true
}
