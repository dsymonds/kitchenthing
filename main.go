package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"gopkg.in/yaml.v2"
)

var (
	configFile = flag.String("config_file", "config.yaml", "configuration `filename`")
	debug      = flag.Bool("debug", false, "whether to log extra information")

	testRender  = flag.String("test_render", "", "`filename` to render a PNG to")
	testTodoist = flag.Bool("test_todoist", false, "whether to use fake Todoist data")
)

type Config struct {
	Font            string        `yaml:"font"`
	RefreshPeriod   time.Duration `yaml:"refresh_period"`
	TodoistAPIToken string        `yaml:"todoist_api_token"`
}

func main() {
	flag.Parse()

	var cfg Config
	cfgRaw, err := ioutil.ReadFile(*configFile)
	if err != nil {
		log.Fatalf("Reading config file %s: %v", *configFile, err)
	}
	if err := yaml.UnmarshalStrict(cfgRaw, &cfg); err != nil {
		log.Fatalf("Parsing config from %s: %v", *configFile, err)
	}

	rend, err := newRenderer(cfg)
	if err != nil {
		log.Fatalf("newRenderer: %v", err)
	}

	if *testRender != "" {
		ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)
		img := image.NewNRGBA(image.Rect(0, 0, 800, 480))
		draw.Draw(img, img.Bounds(), &image.Uniform{color.White}, image.ZP, draw.Src)
		rend.RenderInfo(img, BuildInfo(ctx, cfg))
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

	log.Printf("kitchenthing starting...")
	time.Sleep(500 * time.Millisecond)

	p := newPaper()

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

	if err := p.Start(); err != nil {
		log.Fatalf("Paper start: %v", err)
	}

	log.Printf("kitchenthing startup OK")
	time.Sleep(1 * time.Second)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := loop(ctx, cfg, rend, p); err != nil {
			log.Printf("Loop failed: %v", err)
		}
		cancel()
	}()

	// Wait until interrupted or something else causes a graceful shutdown.
	<-ctx.Done()
	wg.Wait()
	p.Stop()
	log.Printf("kitchenthing done")
}

func loop(ctx context.Context, cfg Config, rend renderer, p paper) error {
	for {
		info := BuildInfo(ctx, cfg)

		p.Init()
		rend.RenderInfo(p, info)
		p.DisplayRefresh()
		p.Sleep()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(info.nextRefresh)):
		}
	}
}

type renderer struct {
	font *opentype.Font

	tiny, small, normal, large, xlarge font.Face
}

func newRenderer(cfg Config) (renderer, error) {
	const dpi = 125 // per paper hardware

	fdata, err := ioutil.ReadFile(cfg.Font)
	if err != nil {
		return renderer{}, fmt.Errorf("loading font file: %w", err)
	}
	font, err := opentype.Parse(fdata)
	if err != nil {
		return renderer{}, fmt.Errorf("parsing font data: %w", err)
	}
	tiny, err := opentype.NewFace(font, &opentype.FaceOptions{ // TODO: need hinting?
		Size: 10, // points
		DPI:  dpi,
	})
	if err != nil {
		return renderer{}, fmt.Errorf("making tiny font face: %w", err)
	}
	small, err := opentype.NewFace(font, &opentype.FaceOptions{ // TODO: need hinting?
		Size: 12, // points
		DPI:  dpi,
	})
	if err != nil {
		return renderer{}, fmt.Errorf("making tiny font face: %w", err)
	}
	normal, err := opentype.NewFace(font, &opentype.FaceOptions{ // TODO: need hinting?
		Size: 16, // points
		DPI:  dpi,
	})
	if err != nil {
		return renderer{}, fmt.Errorf("making tiny font face: %w", err)
	}
	large, err := opentype.NewFace(font, &opentype.FaceOptions{ // TODO: need hinting?
		Size: 20, // points
		DPI:  dpi,
	})
	if err != nil {
		return renderer{}, fmt.Errorf("making tiny font face: %w", err)
	}
	xlarge, err := opentype.NewFace(font, &opentype.FaceOptions{ // TODO: need hinting?
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
	}, nil
}

// Info represents the information to be rendered.
type Info struct {
	today       time.Time
	nextRefresh time.Time

	tasks []renderableTask

	// TODO: report errors?
}

func BuildInfo(ctx context.Context, cfg Config) Info {
	info := Info{
		today:       time.Now(),
		nextRefresh: time.Now().Add(cfg.RefreshPeriod),
	}
	if *testTodoist {
		info.tasks = []renderableTask{
			{4, "something really important", "David", "House"},
			{3, "something important", "", "House"},
			{2, "something nice to do", "", "Other"},
			{1, "if there's time", "", "Other"},
		}
		return info
	}

	tasks, err := TodoistTasks(ctx, cfg)
	if err != nil {
		// TODO: add error to screen? or some sort of simple message?
		log.Printf("Fetching Todoist tasks: %v", err)
	} else {
		info.tasks = tasks
	}

	return info
}

func (r renderer) RenderInfo(dst draw.Image, info Info) {
	// Date in top-right corner.
	next := r.writeText(dst, image.Pt(-2, 2), topLeft, color.Black, r.xlarge, info.today.Format("Mon _2 Jan"))

	var line1 string
	switch n := len(info.tasks); {
	case n == 0:
		line1 = "No tasks remaining for today!"
	case n == 1:
		line1 = "Just one more thing to do:"
	case n == 2:
		line1 = "A couple of tasks to tick off:"
	case n < 5:
		line1 = "A few things that need doing:"
	default:
		line1 = "Quite a bit to get done, eh?"
	}
	next.X = 2
	next = r.writeText(dst, next, topLeft, color.Black, r.large, line1)

	listVPitch := r.normal.Metrics().Height.Ceil()
	listBase := image.Pt(10, next.Y+2+listVPitch) // baseline of each list entry
	for i, task := range info.tasks {             // TODO: adjust font size for task count?
		txt := fmt.Sprintf("[P%d] %s", 4-task.Priority, task.Title)
		if task.Assignee != "" {
			txt += " (" + task.Assignee + ")"
		}
		// TODO: red for overdue?
		baselineY := listBase.Y + i*listVPitch
		origin := image.Pt(listBase.X, baselineY)
		next := r.writeText(dst, origin, bottomLeft, color.Black, r.normal, txt)
		origin = image.Pt(next.X+10, baselineY)
		r.writeText(dst, origin, bottomLeft, colorRed, r.small, task.Project)
	}

	r.writeText(dst, image.Pt(-2, -2), topLeft, color.Black, r.tiny, "Next update: ~"+info.nextRefresh.Format("15:04:05"))
}

var colorRed = color.RGBA{R: 0xFF, G: 0, B: 0, A: 0xFF}

type originAnchor int

const (
	topLeft originAnchor = iota
	bottomLeft
)

// writeText renders some text at the origin.
// If either component of origin is negative, it is interpreted as being relative to the right/bottom.
// It returns the opposite corner.
func (r renderer) writeText(dst draw.Image, origin image.Point, anchor originAnchor, col color.Color, face font.Face, text string) (opposite image.Point) {
	// TODO: fix this to work in case dst's bounds is not (0, 0).

	d := &font.Drawer{
		Dst:  dst,
		Src:  &image.Uniform{col},
		Face: face,
	}

	bounds, advance := d.BoundString(text)
	boundsWidth, boundsHeight := (bounds.Max.X - bounds.Min.X), (bounds.Max.Y - bounds.Min.Y)

	// If the advance is bigger than the bounds, use it.
	if advance > boundsWidth {
		boundsWidth = advance
	}

	dstSize := dst.Bounds().Size()
	lowerRight := fixed.P(dstSize.X-1, dstSize.Y-1)

	// d.Dot needs to end up at the bottom left.
	if origin.X >= 0 {
		// Relative to left side.
		d.Dot.X = fixed.I(origin.X)
	} else {
		// Relative to right side.
		d.Dot.X = lowerRight.X - boundsWidth - fixed.I(-origin.X)
	}
	if origin.Y >= 0 {
		// Relative to top.
		d.Dot.Y = fixed.I(origin.Y)
		if anchor == topLeft {
			d.Dot.Y += boundsHeight
		}
	} else {
		// Relative to bottom.
		d.Dot.Y = lowerRight.Y - fixed.I(-origin.Y)
	}

	d.DrawString(text)

	return image.Pt(d.Dot.X.Round(), d.Dot.Y.Round())
}
