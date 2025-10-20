package main

import (
	"bytes"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dsymonds/todoist"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"gopkg.in/yaml.v2"
)

var (
	configFile = flag.String("config_file", "config.yaml", "configuration `filename`")
	debug      = flag.Bool("debug", false, "whether to log extra information")
	httpFlag   = flag.String("http", "localhost:8080", "`address` on which to serve HTTP")

	actOnMetadata = flag.Bool("act_on_metadata", false, "whether to act on metadata in task labels")

	testRender  = flag.String("test_render", "", "`filename` to render a PNG to")
	testTodoist = flag.Bool("test_todoist", false, "whether to use fake Todoist data")
	usePaper    = flag.Bool("use_paper", true, "whether to interact with ePaper")
)

type Config struct {
	Font            string        `yaml:"font"`
	RefreshPeriod   time.Duration `yaml:"refresh_period"`
	TodoistAPIToken string        `yaml:"todoist_api_token"`
	PhotosDir       string        `yaml:"photos_dir"`

	Alertmanager  string `yaml:"alertmanager"`
	MQTT          string `yaml:"mqtt"`
	HomeAssistant struct {
		Addr     string `yaml:"addr"`
		Token    string `yaml:"token"`
		Template string `yaml:"template"`
	} `yaml:"home_assistant"`

	Orderings []struct {
		Project string          `yaml:"project"`
		Groups  []GroupPatterns `yaml:"groups"`
	} `yaml:"orderings"`

	// Messages are applied in a first-match order.
	Messages []message `yaml:"messages"`
}

type message struct {
	// One of these should normally be set.
	// If none are set, this message matches all.
	Eq *int `yaml:"eq"` // ==
	Lt *int `yaml:"lt"` // <

	Options []string `yaml:"options"`
}

func (m message) Matches(n int) bool {
	if m.Eq != nil {
		return n == *m.Eq
	}
	if m.Lt != nil {
		return n < *m.Lt
	}
	return true
}

func parseConfig(filename string) (Config, error) {
	raw, err := ioutil.ReadFile(filename)
	if err != nil {
		return Config{}, fmt.Errorf("reading config file %s: %v", filename, err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing config from %s: %v", filename, err)
	}
	return cfg, nil
}

func main() {
	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	cfg, err := parseConfig(*configFile)
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		startTime: time.Now(),
		cfg:       cfg,
	}
	http.Handle("/", s)

	rend, err := newRenderer(cfg, s.pickPhoto)
	if err != nil {
		log.Fatalf("newRenderer: %v", err)
	}
	ref, err := newRefresher(cfg)
	if err != nil {
		log.Fatalf("newRefresher: %v", err)
	}

	if *testRender != "" {
		ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)
		img := image.NewPaletted(image.Rect(0, 0, 800, 480), staticPalette)
		draw.Draw(img, img.Bounds(), &image.Uniform{color.White}, image.ZP, draw.Src)
		rend.Render(img, ref.Refresh(ctx))
		var buf bytes.Buffer
		if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
			log.Fatalf("Encoding PNG: %v", err)
		}
		if err := ioutil.WriteFile(*testRender, buf.Bytes(), 0644); err != nil {
			log.Fatalf("Writing render: %v", err)
		}
		log.Printf("Wrote render to %s (%d bytes)", *testRender, buf.Len())
		return
	}

	log.SetOutput(io.MultiWriter(os.Stderr, s))
	log.Printf("kitchenthing starting...")
	time.Sleep(500 * time.Millisecond)

	p := newPaper() // doesn't interact with paper

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())

	// Handle signals.
	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, os.Interrupt) // TODO: others?

		sig := <-sigc
		log.Printf("Caught signal %v; shutting down gracefully", sig)
		cancel()
	}()

	// Start HTTP server.
	httpServer := &http.Server{}
	wg.Add(1)
	go func() {
		defer wg.Done()

		l, err := net.Listen("tcp", *httpFlag)
		if err != nil {
			log.Printf("net.Listen(_, %q): %v", *httpFlag, err)
			cancel()
		}

		log.Printf("Serving HTTP on %s", l.Addr())
		err = httpServer.Serve(l)
		if err != http.ErrServerClosed {
			log.Printf("http.Serve: %v", err)
			cancel()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()

		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	mqtt, err := NewMQTT(cfg)
	if err != nil {
		log.Fatalf("MQTT: %v", err)
	}

	if *usePaper {
		if err := p.Start(); err != nil {
			log.Fatalf("Paper start: %v", err)
		}
	}

	// Wait a bit. If things are still okay, consider this a successful startup.
	select {
	case <-ctx.Done():
		goto exit
	case <-time.After(2 * time.Second):
	}

	log.Printf("kitchenthing startup OK")
	time.Sleep(1 * time.Second)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := loop(ctx, cfg, rend, ref, p, mqtt); err != nil {
			log.Printf("Loop failed: %v", err)
		}
		cancel()
	}()

	// Wait until interrupted or something else causes a graceful shutdown.
exit:
	<-ctx.Done()
	wg.Wait()
	if *usePaper {
		p.Stop()
	}
	log.Printf("kitchenthing done")
}

type server struct {
	startTime time.Time
	cfg       Config

	mu        sync.Mutex
	logBuf    bytes.Buffer
	nextPhoto string
}

func (s *server) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err = s.logBuf.Write(p)

	// Shrink to stay in a sensible bounds.
	const max = 100 << 10 // 100 KB should be plenty.
	if s.logBuf.Len() > max {
		b := s.logBuf.Bytes()
		for len(b) > max {
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				b = nil
				break
			}
			b = b[i+1:]
		}
		copy(s.logBuf.Bytes(), b)
		s.logBuf.Truncate(len(b))
	}

	return
}

func (s *server) pickPhoto() (string, error) {
	if s.cfg.PhotosDir == "" {
		return "", nil
	}
	opts, err := photoOptions(s.cfg.PhotosDir)
	if err != nil {
		return "", err
	}
	if len(opts) == 0 {
		return "", fmt.Errorf("no files in photos dir")
	}

	// Use a previously-selected photo.
	// Always do this here so we can validate against the real files,
	// which avoids any risk of an attack making us load another file.
	s.mu.Lock()
	sel := s.nextPhoto
	s.nextPhoto = ""
	s.mu.Unlock()
	if sel != "" {
		for _, opt := range opts {
			if sel == opt {
				log.Printf("Using previously selected photo %q", sel)
				return sel, nil
			}
		}
		log.Printf("Error: previously selected photo %q does not exist; ignoring", sel)
	}

	return opts[rand.Intn(len(opts))], nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	default:
		http.NotFound(w, r)
	case "/":
		s.serveFront(w, r)
	case "/set-next-photo":
		s.serveSetNextPhoto(w, r)
	}
}

func (s *server) serveFront(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Uptime time.Duration
		Logs   string
		Photos []string
	}{
		Uptime: time.Since(s.startTime).Truncate(time.Minute),
	}

	s.mu.Lock()
	data.Logs = s.logBuf.String()
	s.mu.Unlock()

	if s.cfg.PhotosDir != "" {
		var err error
		data.Photos, err = photoOptions(s.cfg.PhotosDir)
		if err != nil {
			log.Printf("Looking for photo options: %v", err)
			// Continue anyway.
		}
	}

	var buf bytes.Buffer
	if err := frontHTMLTmpl.Execute(&buf, data); err != nil {
		log.Printf("Executing template: %v", err)
		http.Error(w, "Internal error executing template: "+err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.Copy(w, &buf)
}

//go:embed front.html.tmpl
var frontHTML string

var frontHTMLTmpl = template.Must(template.New("front").Parse(frontHTML))

func (s *server) serveSetNextPhoto(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	sel := r.PostFormValue("photo")

	// In theory we should do an XSRF check here, but the threat model isn't worth the effort.

	s.mu.Lock()
	s.nextPhoto = sel
	s.mu.Unlock()
	log.Printf("Selected %q as the next photo to use", sel)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func loop(ctx context.Context, cfg Config, rend renderer, ref *refresher, p paper, mqtt *MQTT) error {
	var prev displayData
	for {
		data := ref.Refresh(ctx)

		if !data.Equal(prev) {
			log.Printf("New data to be displayed; refreshing now")

			if mqtt != nil {
				if err := mqtt.PublishUpdate(data.tasks); err != nil {
					log.Printf("MQTT publish: %v", err)
				}
			}

			if *usePaper {
				p.Init()
				rend.Render(p, data)
				p.DisplayRefresh()
				p.Sleep()
			}
			prev = data
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.RefreshPeriod):
		}
	}
}

type renderer struct {
	font *opentype.Font

	tiny, small, normal, large, xlarge font.Face

	photoPicker func() (string, error)

	messages []message
}

func newRenderer(cfg Config, photoPicker func() (string, error)) (renderer, error) {
	const dpi = 125 // per paper hardware

	fdata, err := ioutil.ReadFile(cfg.Font)
	if err != nil {
		return renderer{}, fmt.Errorf("loading font file: %w", err)
	}
	font, err := opentype.Parse(fdata)
	if err != nil {
		return renderer{}, fmt.Errorf("parsing font data: %w", err)
	}
	tiny, err := opentype.NewFace(font, &opentype.FaceOptions{
		Size: 10, // points
		DPI:  dpi,
	})
	if err != nil {
		return renderer{}, fmt.Errorf("making tiny font face: %w", err)
	}
	small, err := opentype.NewFace(font, &opentype.FaceOptions{
		Size: 12, // points
		DPI:  dpi,
	})
	if err != nil {
		return renderer{}, fmt.Errorf("making tiny font face: %w", err)
	}
	normal, err := opentype.NewFace(font, &opentype.FaceOptions{
		Size: 16, // points
		DPI:  dpi,
	})
	if err != nil {
		return renderer{}, fmt.Errorf("making tiny font face: %w", err)
	}
	large, err := opentype.NewFace(font, &opentype.FaceOptions{
		Size: 20, // points
		DPI:  dpi,
	})
	if err != nil {
		return renderer{}, fmt.Errorf("making tiny font face: %w", err)
	}
	xlarge, err := opentype.NewFace(font, &opentype.FaceOptions{
		Size: 36, // points
		DPI:  dpi,
	})
	if err != nil {
		return renderer{}, fmt.Errorf("making tiny font face: %w", err)
	}
	return renderer{
		font: font,

		tiny:   tiny,
		small:  small,
		normal: normal,
		large:  large,
		xlarge: xlarge,

		photoPicker: photoPicker,

		messages: cfg.Messages,
	}, nil
}

type refresher struct {
	cfg Config
	ts  *todoist.Syncer

	reorderers map[string]*Reorderer

	// lastOpenTasks is a set of Todoist task IDs of tasks that were open
	// last time ts.Sync ran. This is used to detect tasks that get completed.
	lastOpenTasks map[string]todoist.Task
}

func newRefresher(cfg Config) (*refresher, error) {
	r := &refresher{
		cfg: cfg,
		ts:  todoist.NewSyncer(cfg.TodoistAPIToken),

		reorderers: make(map[string]*Reorderer),
	}
	for _, o := range cfg.Orderings {
		ro, err := NewReorderer(o.Groups)
		if err != nil {
			return nil, fmt.Errorf("creating Reorderer for project %q: %w", o.Project, err)
		}
		r.reorderers[o.Project] = ro
		log.Printf("Prepared reorderer for project %q with %d groups", o.Project, len(o.Groups))
	}

	return r, nil
}

type displayData struct {
	today time.Time // only day resolution

	tasks []renderableTask

	// TODO: report errors?

	alerts []Alert
	hass   string
}

func (dd displayData) Equal(o displayData) bool {
	if !dd.today.Equal(o.today) {
		return false
	}
	if len(dd.tasks) != len(o.tasks) {
		return false
	}
	for i := range dd.tasks {
		if dd.tasks[i].Compare(o.tasks[i]) != 0 {
			return false
		}
	}
	if len(dd.alerts) != len(o.alerts) {
		return false
	}
	for i := range dd.alerts {
		if !dd.alerts[i].Same(o.alerts[i]) {
			return false
		}
	}
	if dd.hass != o.hass {
		return false
	}
	return true
}

func (r *refresher) Refresh(ctx context.Context) displayData {
	d, m, y := time.Now().Date()
	dd := displayData{
		today: time.Date(d, m, y, 0, 0, 0, 0, time.Local),
	}
	if *testTodoist {
		t0 := time.Time{}
		tset := dd.today.Add(17*time.Hour + 30*time.Minute) // 5:30pm
		dd.tasks = []renderableTask{
			{Priority: 4, Time: t0, Title: "something really important", Assignee: "David", Project: "House", Done: 1, Total: 3},
			{Priority: 3, Time: tset, Title: "something important", HasDesc: true, Project: "House", InProgress: true},
			{Priority: 2, Time: t0, Title: "something nice to do", Overdue: true, Project: "Other"},
			{Priority: 1, Time: t0, Title: "if there's time", Project: "Other", Done: 0, Total: 4},
		}
		return dd
	}

	if err := r.ts.Sync(ctx); err != nil {
		// TODO: add error to screen? or some sort of simple message?
		log.Printf("Syncing from Todoist: %v", err)
		// Continue on and use any existing data.
	}
	newOpen, closed := make(map[string]todoist.Task), r.lastOpenTasks
	for id, task := range r.ts.Tasks {
		// this is actually only the open tasks, but be defensive.
		if task.Checked {
			log.Printf("Woah! Task %q remains in ts.Tasks, but is checked!", task.Content)
			continue
		}
		newOpen[id] = task
		delete(closed, id)
	}
	r.lastOpenTasks = newOpen

	dd.tasks = RenderableTasks(r.ts)
	ApplyMetadata(ctx, r.ts, *actOnMetadata)
	r.reorder(ctx)

	if r.cfg.Alertmanager != "" {
		as, err := FetchAlerts(ctx, r.cfg.Alertmanager)
		if err != nil {
			log.Printf("Fetching alerts from Alertmanager %s: %v", r.cfg.Alertmanager, err)
		} else {
			dd.alerts = as
		}
	}

	if hacfg := r.cfg.HomeAssistant; hacfg.Addr != "" {
		hass := HASS{addr: hacfg.Addr, token: hacfg.Token}

		ha, err := hass.RenderTemplate(ctx, hacfg.Template)
		if err != nil {
			log.Printf("Querying HomeAssistant: %v", err)
		} else {
			dd.hass = ha
		}

		for _, task := range closed {
			data := struct {
				Content string `json:"content"`
				Project string `json:"project"`
			}{
				Content: task.Content,
				Project: r.ts.Projects[task.ProjectID].Name,
			}
			if err := hass.FireEvent(ctx, "todoist_task_completed", data); err != nil {
				log.Printf("Firing HASS event: %v", err)
			}
		}
	}

	return dd
}

func (r *refresher) reorder(ctx context.Context) {
	type ot struct { // ordered task
		ID         string
		Content    string
		Labels     []string
		ChildOrder int // current child_order
	}

	for project, ro := range r.reorderers {
		var tasks []ot
		for _, task := range r.ts.Tasks {
			if r.ts.Projects[task.ProjectID].Name != project {
				continue
			}
			if task.ParentID != "" {
				continue
			}
			tasks = append(tasks, ot{task.ID, task.Content, task.Labels, task.ChildOrder})
		}
		// First put them in their current order.
		sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].ChildOrder < tasks[j].ChildOrder })
		// Figure out the desired arrangement.
		arr := ro.Arrange(len(tasks), func(i int) string { return tasks[i].Content })
		// Any label adjustments to make?
		for i, x := range arr.New {
			task := tasks[x]
			want := "" // what s: label should this task have?
			if i < len(arr.Groups) {
				want = "s:" + arr.Groups[i]
			}
			seen := false   // whether want!="" and we've seen it
			update := false // whether to update the task
			for i := 0; i < len(task.Labels); {
				label := task.Labels[i]
				if !strings.HasPrefix(label, "s:") {
					i++
					continue // not ours to touch
				}
				if label != want {
					copy(task.Labels[i:], task.Labels[i+1:])
					task.Labels = task.Labels[:len(task.Labels)-1]
					update = true
					continue
				}
				seen = true
				i++
			}
			if want != "" && !seen {
				task.Labels = append(task.Labels, want)
				update = true
			}
			if !update {
				continue
			}
			if err := r.ts.UpdateTask(ctx, task.ID, todoist.TaskUpdates{Labels: &task.Labels}); err != nil {
				log.Printf("UpdateTask: %v", err)
				continue
			}
			log.Printf("Updated %q to this label set: %q", task.Content, task.Labels)
		}
		// Are any changes required?
		changes := false
		var ids []string // new order of task IDs
		for i, x := range arr.New {
			if i != x {
				changes = true
			}
			ids = append(ids, tasks[x].ID)
		}
		if !changes {
			continue
		}
		if err := r.ts.Reorder(ctx, ids); err != nil {
			log.Printf("Reordering project %q: %v", project, err)
			continue
		}
		log.Printf("Reordered project %q!", project)
	}
}

func (r renderer) Render(dst draw.Image, data displayData) {
	// Date in top-right corner.
	// Put date number in red for December, before day 25.
	var domCol color.Color = color.Black
	_, mon, day := data.today.Date()
	if mon == time.December && day <= 25 {
		domCol = colorRed
	}
	monBL := r.writeText(dst, image.Pt(-2, 2), topRight, color.Black, r.xlarge, data.today.Format(" Jan"))
	domBL := r.writeText(dst, image.Pt(monBL.X, 2), topRight, domCol, r.xlarge, data.today.Format(" 2"))
	dateBL := r.writeText(dst, image.Pt(domBL.X, 2), topRight, color.Black, r.xlarge, data.today.Format("Mon"))

	var subtitles []string
	for _, msg := range r.messages {
		if msg.Matches(len(data.tasks)) {
			subtitles = msg.Options
			break
		}
	}
	subtitle := subtitles[rand.Intn(len(subtitles))]
	next := image.Pt(10, dateBL.Y)
	r.writeText(dst, next, bottomLeft, color.Black, r.large, subtitle)
	next = image.Pt(2, dateBL.Y)

	// Render footer first, so we know where to stop rendering tasks to avoid overlap.
	topOfFooterY := dst.Bounds().Max.Y - 4
	// Put HASS template data at the very bottom, if present.
	if data.hass != "" {
		hassFont := r.small
		vPitch := hassFont.Metrics().Height.Ceil()
		origin := image.Pt(2, topOfFooterY)
		r.writeText(dst, origin, bottomLeft, color.Black, hassFont, data.hass)
		topOfFooterY -= vPitch
	}
	// Render alerts from the bottom up.
	alertFont := r.tiny
	alertListVPitch := alertFont.Metrics().Height.Ceil()
	for i := len(data.alerts) - 1; i >= 0; i-- {
		alert := data.alerts[i]
		origin := image.Pt(2, topOfFooterY)
		next := r.writeText(dst, origin, bottomLeft, colorRed, alertFont, alert.Summary)
		origin.X = next.X
		r.writeText(dst, origin, bottomLeft, color.Black, alertFont, ": "+alert.Description)

		topOfFooterY -= alertListVPitch
	}

	listVPitch := r.normal.Metrics().Height.Ceil()
	listBase := image.Pt(10, next.Y+2+listVPitch) // baseline of each list entry
	hiddenTasks := 0
	for i, task := range data.tasks { // TODO: adjust font size for task count?
		baselineY := listBase.Y + i*listVPitch
		origin := image.Pt(listBase.X, baselineY)

		if baselineY >= topOfFooterY {
			// Would overlap with alerts/HASS.
			hiddenTasks = len(data.tasks) - i
			break
		}

		var titleCol color.Color = color.Black
		if task.Overdue {
			titleCol = colorRed
		}

		txt := fmt.Sprintf("[P%d] %s", 4-task.Priority, task.Title)
		// Priority
		next := r.writeText(dst, origin, bottomLeft, color.Black, r.normal, fmt.Sprintf("[P%d] ", 4-task.Priority))
		origin = image.Pt(next.X, baselineY)

		// Title
		next = r.writeText(dst, origin, bottomLeft, titleCol, r.normal, task.Title)
		origin = image.Pt(next.X, baselineY)

		// Remaining info
		txt = ""
		if task.Total > 0 {
			txt += fmt.Sprintf(" {%d/%d}", task.Done, task.Total)
		}
		if task.HasDesc {
			txt += " ♫"
		}
		if task.InProgress {
			txt += " ◊"
		}
		if !task.Time.IsZero() {
			txt += " <" + task.Time.Format(time.Kitchen) + ">"
		}
		if task.Assignee != "" {
			txt += " (" + task.Assignee + ")"
		}
		next = r.writeText(dst, origin, bottomLeft, color.Black, r.normal, txt)
		origin = image.Pt(next.X+10, baselineY)
		r.writeText(dst, origin, bottomLeft, colorRed, r.small, task.Project)
	}
	bottomOfListY := listBase.Y + (len(data.tasks)-hiddenTasks-1)*listVPitch

	if hiddenTasks > 0 {
		origin := image.Pt(dst.Bounds().Max.X-2, dst.Bounds().Max.Y-2)
		noun := "task"
		if hiddenTasks != 1 {
			noun = "tasks"
		}
		msg := fmt.Sprintf("%d %s hidden", hiddenTasks, noun)
		r.writeText(dst, origin, bottomRight, colorRed, r.tiny, msg)
	}

	sub := clippedImage{
		img: dst,
		bounds: image.Rectangle{
			Min: image.Pt(10, bottomOfListY+10),
			Max: image.Pt(dst.Bounds().Max.X-10, topOfFooterY-2),
		},
	}
	if !sub.bounds.Empty() {
		photo, err := r.photoPicker()
		if err != nil {
			log.Printf("Picking random photo: %v", err)
		} else if photo != "" {
			if err := drawPhoto(sub, photo); err != nil {
				log.Printf("Drawing random photo: %v", err)
			}
		}
	}
}

type originAnchor int

const (
	topLeft originAnchor = iota
	topRight
	bottomLeft
	bottomRight
)

func (oa originAnchor) String() string {
	switch oa {
	case topLeft:
		return "TL"
	case topRight:
		return "TR"
	case bottomLeft:
		return "BL"
	case bottomRight:
		return "BR"
	default:
		return "???"
	}
}

// writeText renders some text at the origin.
// If either component of origin is negative, it is interpreted as being relative to the right/bottom.
// The text is written such that the origin is at the given anchor corner of the text.
// It returns the opposite corner.
func (r renderer) writeText(dst draw.Image, origin image.Point, anchor originAnchor, col color.Color, face font.Face, text string) (opposite image.Point) {
	defer func() {
		if *debug {
			log.Printf("writeText(origin=%v, anchor=%v, text=%q) -> %v", origin, anchor, text, opposite)
		}
	}()
	// TODO: fix this to work in case dst's bounds is not (0, 0).
	// TODO: It'd be nice to log a message if the text busts the bounds of dst.

	d := &font.Drawer{
		Dst:  dst,
		Src:  &image.Uniform{col},
		Face: face,
	}

	// Figure out the dimensions of the text to draw.
	// This is not the strict bounds (e.g. in the case of descenders), but we match to baselines
	// so that text can be aligned in a single line.
	// Ascent is -bounds.Min.Y, and descent (which we ignore) is bounds.Max.Y.
	// Always use the advance to get to where the next glyph should go.
	bounds, advance := d.BoundString(text)
	drawWidth, drawHeight := advance, -bounds.Min.Y

	if *debug {
		log.Printf("writeText: bounds of %q: = %v (ascent=%d descent=%d)", text, bounds, -bounds.Min.Y, bounds.Max.Y)
	}

	dstSize := dst.Bounds().Size()
	lowerRight := fixed.P(dstSize.X-1, dstSize.Y-1)

	// Compute where the root of the text should go (d.Dot), which should be the bottom left.
	// d.Dot needs to end up at the bottom left, since that's what DrawString orients around.
	// We need to translate based on origin and anchor.
	if origin.X >= 0 {
		// Relative to left side.
		d.Dot.X = fixed.I(origin.X)
	} else {
		// Relative to right side.
		d.Dot.X = lowerRight.X - fixed.I(-origin.X)
	}
	if origin.Y >= 0 {
		// Relative to top.
		d.Dot.Y = fixed.I(origin.Y)
	} else {
		// Relative to bottom.
		d.Dot.Y = lowerRight.Y - fixed.I(-origin.Y)
	}
	switch anchor {
	case topLeft:
		d.Dot.Y += drawHeight
	case topRight:
		d.Dot.X -= drawWidth
		d.Dot.Y += drawHeight
	case bottomLeft:
		// correct already
	case bottomRight:
		d.Dot.X -= drawWidth
	}

	if *debug {
		log.Printf("writeText: baseline location: Dot=%v", d.Dot)
	}
	d.DrawString(text)

	// d.Dot is now at the bottom right corner.
	// Adjust what we return so we always give back the corner
	// opposite of the provided anchor.
	switch anchor {
	case topLeft: // return bottom right
		// correct already
	case topRight: // return bottom left
		d.Dot.X -= drawWidth
	case bottomLeft: // return top right
		d.Dot.Y -= drawHeight
	case bottomRight: // return top left
		d.Dot.X -= drawWidth
		d.Dot.Y -= drawHeight
	}

	return image.Pt(d.Dot.X.Round(), d.Dot.Y.Round())
}

func photoOptions(dir string) ([]string, error) {
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("os.UserHomeDir: %w", err)
		}
		dir = filepath.Join(home, dir[2:])
	}

	opts, err := filepath.Glob(filepath.Join(dir, "*.jpg"))
	if err != nil {
		return nil, fmt.Errorf("globbing photos dir: %w", err)
	}
	return opts, nil
}

func drawPhoto(dst draw.Image, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("opening %s: %w", filename, err)
	}
	src, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("decoding image %s: %w", filename, err)
	}

	srcWidth := src.Bounds().Max.X - src.Bounds().Min.X
	srcHeight := src.Bounds().Max.Y - src.Bounds().Min.Y
	dstWidth := dst.Bounds().Max.X - dst.Bounds().Min.X
	dstHeight := dst.Bounds().Max.Y - dst.Bounds().Min.Y
	scaleWidth := float64(srcWidth) / float64(dstWidth)
	scaleHeight := float64(srcHeight) / float64(dstHeight)
	var scale float64
	if scaleWidth >= scaleHeight {
		// Width needs more shrinking.
		// Shift vertically to centre.
		scale = scaleWidth
		// TODO
	} else {
		// Height needs more shrinking.
		// Shift horizontally to centre.
		scale = scaleHeight
		newWidth := int(float64(srcWidth) / scaleHeight)
		offset := (dstWidth - newWidth) / 2
		dst = clippedImage{
			img: dst,
			bounds: image.Rectangle{
				Min: image.Pt(dst.Bounds().Min.X+offset, dst.Bounds().Min.Y),
				Max: image.Pt(dst.Bounds().Max.X-offset, dst.Bounds().Max.Y),
			},
		}
	}

	// To make the remaining code simpler, shift dst so that its bounds always starts at (0, 0).
	dst = shiftedImage{dst}

	// TODO: This is quite inefficient.
	carriedErrors := make([]colorError, dst.Bounds().Max.X*dst.Bounds().Max.Y)
	carriedError := func(x, y int) *colorError {
		return &carriedErrors[x+y*dst.Bounds().Max.X]
	}
	for y := 0; y < dst.Bounds().Max.Y; y++ {
		for x := 0; x < dst.Bounds().Max.X; x++ {
			srcX := src.Bounds().Min.X + int(scale*float64(x))
			srcY := src.Bounds().Min.Y + int(scale*float64(y))
			srcCol := src.At(srcX, srcY)
			srcCol = carriedError(x, y).Apply(srcCol)
			dstCol := dst.ColorModel().Convert(srcCol)
			dst.Set(x, y, dstCol)

			ce := colorSub(dstCol, srcCol)

			if x+1 < dst.Bounds().Max.X {
				carriedError(x+1, y).Add(ce.Mul(7.0 / 16))
			}
			if x-1 >= 0 && y+1 < dst.Bounds().Max.Y {
				carriedError(x-1, y+1).Add(ce.Mul(3.0 / 16))
			}
			if y+1 < dst.Bounds().Max.Y {
				carriedError(x, y+1).Add(ce.Mul(5.0 / 16))
			}
			if x+1 < dst.Bounds().Max.X && y+1 < dst.Bounds().Max.Y {
				carriedError(x+1, y+1).Add(ce.Mul(1.0 / 16))
			}
		}
	}

	return nil
}

type clippedImage struct {
	img    draw.Image
	bounds image.Rectangle
}

func (ci clippedImage) ColorModel() color.Model     { return ci.img.ColorModel() }
func (ci clippedImage) Bounds() image.Rectangle     { return ci.bounds }
func (ci clippedImage) At(x, y int) color.Color     { return ci.img.At(x, y) }
func (ci clippedImage) Set(x, y int, c color.Color) { ci.img.Set(x, y, c) }

// shiftedImage wraps a draw.Image to make the bounds always start at (0, 0).
type shiftedImage struct {
	img draw.Image
}

func (si shiftedImage) ColorModel() color.Model { return si.img.ColorModel() }
func (si shiftedImage) Bounds() image.Rectangle {
	return image.Rectangle{
		Max: image.Pt(
			si.img.Bounds().Max.X-si.img.Bounds().Min.X,
			si.img.Bounds().Max.Y-si.img.Bounds().Min.Y,
		),
	}
}
func (si shiftedImage) At(x, y int) color.Color {
	return si.img.At(x+si.img.Bounds().Min.X, y+si.img.Bounds().Min.Y)
}
func (si shiftedImage) Set(x, y int, c color.Color) {
	si.img.Set(x+si.img.Bounds().Min.X, y+si.img.Bounds().Min.Y, c)
}

type colorError [3]int32 // RGB; each in range [-0xffff, 0xffff]

// Add adds the new error to this error, saturating correctly.
func (ce *colorError) Add(x colorError) {
	ce[0] = clipTo16(ce[0] + x[0])
	ce[1] = clipTo16(ce[1] + x[1])
	ce[2] = clipTo16(ce[2] + x[2])
}

// Mul returns a scaled version of the colorError. It assumes x is in [0,1].
func (ce colorError) Mul(x float64) colorError {
	return colorError{int32(x * float64(ce[0])), int32(x * float64(ce[1])), int32(x * float64(ce[2]))}
}

// Apply applies the error to a given color.
func (ce colorError) Apply(x color.Color) color.Color {
	r, g, b, _ := x.RGBA()
	return color.RGBA64{
		clipToU16(int32(r) + ce[0]),
		clipToU16(int32(g) + ce[1]),
		clipToU16(int32(b) + ce[2]),
		0xFFFF,
	}
}

// colorSub returns b-a.
func colorSub(a, b color.Color) colorError {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return colorError{
		int32(br) - int32(ar),
		int32(bg) - int32(ag),
		int32(bb) - int32(ab),
	}
}

func clipTo16(x int32) int32 {
	if x < -0xffff {
		return -0xffff
	}
	if x > 0xffff {
		return 0xffff
	}
	return x
}

func clipToU16(x int32) uint16 {
	if x < 0 {
		return 0
	}
	if x > 0xffff {
		return 0xffff
	}
	return uint16(x)
}
