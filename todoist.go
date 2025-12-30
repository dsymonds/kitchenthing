package main

// Todoist integration.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
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

func ApplyMetadata(ctx context.Context, ts *todoist.Syncer, cfg Config, mutate bool) {
	for _, task := range ts.Tasks {
		for _, label := range task.Labels {
			if strings.HasPrefix(label, "m:") {
				if err := applyMetadata(ctx, ts, cfg, task, label, mutate); err != nil {
					log.Printf("Applying metadata label %q to task %s (%q): %v", label, task.ID, task.Content, err)
				}
			}
		}
	}
}

func applyMetadata(ctx context.Context, ts *todoist.Syncer, cfg Config, task todoist.Task, label string, mutate bool) error {
	switch {
	case label == "m:uf":
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
		if err := removeLabel(ctx, ts, task, "in-progress", mutate); err != nil {
			return err
		}

		return nil
	case label == "m:dd":
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
	case strings.HasPrefix(label, "m:rem="):
		// Add reminder for the user this task is assigned to.

		// Only reminders for assigned tasks, and tasks with a due date of today.
		if task.Responsible == nil || task.Due == nil || task.Due.When() != 0 {
			return nil
		}

		val := label[6:] // skip over "m:rem="
		want, err := reminder(cfg, task, val)
		if err != nil {
			return err
		}

		// If this is relative, and it's too late, just delete the label.
		if want.Type == "relative" {
			t, ok := task.Due.Time()
			if !ok {
				return nil
			}
			if int(time.Until(t).Minutes()) <= *want.MinuteOffset {
				return removeLabel(ctx, ts, task, label, mutate)
			}
		}

		// Add if there isn't already an equivalent reminder.
		// TODO: Inspect IsDeleted flag?
		equiv := false
		for _, rem := range ts.Reminders {
			if equivReminders(rem, want) {
				equiv = true
				break
			}
		}
		if !equiv {
			if !mutate {
				log.Printf("Would add reminder %+v to task %q", want, task.Content)
			} else {
				if err := ts.AddReminder(ctx, want); err != nil {
					return fmt.Errorf("adding reminder: %w", err)
				}
				log.Printf("Added reminder to %q", task.Content)
			}
		}

		// Remove this label.
		if err := removeLabel(ctx, ts, task, label, mutate); err != nil {
			return err
		}
	}

	return nil
}

// reminder creates the desired reminder for the task.
// val is either a relative duration like "30m", or a location ID.
func reminder(cfg Config, task todoist.Task, val string) (todoist.Reminder, error) {
	// Prefer a location ID.
	loc, ok := cfg.Locations[val]
	if ok {
		return todoist.Reminder{
			TaskID: task.ID,
			UserID: *task.Responsible,
			Type:   "location",

			Name:      loc.Name,
			Latitude:  strconv.FormatFloat(loc.Latitude, 'f', -1, 64),
			Longitude: strconv.FormatFloat(loc.Longitude, 'f', -1, 64),
			Radius:    loc.Radius,
		}, nil
	}

	d, err := time.ParseDuration(val)
	if err != nil {
		return todoist.Reminder{}, fmt.Errorf("could not parse m:rem value %q: %w", val, err)
	}

	mins := int(d.Minutes())
	return todoist.Reminder{
		TaskID:       task.ID,
		UserID:       *task.Responsible,
		Type:         "relative",
		MinuteOffset: &mins,
	}, nil
}

func equivReminders(a, b todoist.Reminder) bool {
	// TODO: support location-based reminders.
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

func removeLabel(ctx context.Context, ts *todoist.Syncer, task todoist.Task, remove string, mutate bool) error {
	labels := []string{} // Todoist wants an empty slice to end up with zero labels.
	for _, label := range task.Labels {
		if label != remove {
			labels = append(labels, label)
		}
	}
	if len(labels) == len(task.Labels) {
		return nil
	}
	if !mutate {
		log.Printf("Would change label set from %v to %v", task.Labels, labels)
		return nil
	}
	err := ts.UpdateTask(ctx, task.ID, todoist.TaskUpdates{Labels: &labels})
	if err != nil {
		return fmt.Errorf("removing labels: %w", err)
	}
	log.Printf("Changed label set from %v to %v", task.Labels, labels)
	return nil
}
