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

	"github.com/golang/freetype"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/math/fixed"
	"gopkg.in/yaml.v2"
)

var (
	configFile = flag.String("config_file", "config.yaml", "configuration `filename`")
	debug      = flag.Bool("debug", false, "whether to log extra information")

	testRender = flag.String("test_render", "", "`filename` to render a PNG to")
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
	font *truetype.Font
}

func newRenderer(cfg Config) (renderer, error) {
	fdata, err := ioutil.ReadFile(cfg.Font)
	if err != nil {
		return renderer{}, fmt.Errorf("loading font file: %w", err)
	}
	font, err := freetype.ParseFont(fdata)
	if err != nil {
		return renderer{}, fmt.Errorf("parsing font data: %w", err)
	}
	return renderer{
		font: font,
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
	// TODO: the text layout here is a bit rubbish.

	// Date in top-right corner.
	writeText(dst, freetype.Pt(420, 50), color.Black, 36, r.font, info.today.Format("Mon _2 Jan"))

	var line1 string
	switch n := len(info.tasks); {
	case n == 0:
		line1 = "No tasks for today!"
	case n == 1:
		line1 = "Just one more thing to do:"
	case n == 2:
		line1 = "A couple of tasks to tick off:"
	case n <= 5:
		line1 = "A few things that need doing:"
	default:
		line1 = "Quite a bit to get done, eh?"
	}
	writeText(dst, freetype.Pt(2, 100), color.Black, 20, r.font, line1)
	for i, task := range info.tasks {
		txt := "◊ " + task.Title
		if task.Assignee != "" {
			txt += " (" + task.Assignee + ")"
		}
		// TODO: red for overdue?
		// TODO: adjust size for task count?
		p := freetype.Pt(10, 110+28*(i+1)) // TODO: carry this instead
		p = writeText(dst, p, color.Black, 16, r.font, txt)
		p = p.Add(freetype.Pt(10, 0)) // nudge over
		p = writeText(dst, p, colRed.RGBA(), 12, r.font, task.Project)
	}

	writeText(dst, freetype.Pt(640, 470), color.Black, 8, r.font, "Next update: ~"+info.nextRefresh.Format("15:04:05"))
}

func writeText(dst draw.Image, p fixed.Point26_6, col color.Color, fontSize float64, font *truetype.Font, text string) fixed.Point26_6 {
	ctx := freetype.NewContext()
	ctx.SetDst(dst)
	ctx.SetDPI(125)
	ctx.SetClip(dst.Bounds())
	ctx.SetFont(font)
	ctx.SetFontSize(fontSize)
	ctx.SetSrc(&image.Uniform{col})
	np, err := ctx.DrawString(text, p)
	if err != nil {
		log.Printf("Writing text: %v", err)
		return p
	}
	return np
}
