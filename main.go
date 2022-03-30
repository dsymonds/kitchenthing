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
	"gopkg.in/yaml.v2"
)

var (
	configFile = flag.String("config_file", "config.yaml", "configuration `filename`")
	debug      = flag.Bool("debug", false, "whether to log extra information")

	testRender = flag.String("test_render", "", "`filename` to render a PNG to")
)

type Config struct {
	Font            string `yaml:"font"`
	TodoistAPIToken string `yaml:"todoist_api_token"`
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
		rend.refresh(ctx, img)
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
		if err := loop(ctx, rend, p); err != nil {
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

func loop(ctx context.Context, rend renderer, p paper) error {
	for {
		p.Init()
		rend.refresh(ctx, p)
		p.DisplayRefresh()
		p.Sleep()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Minute):
		}
	}
}

type renderer struct {
	cfg  Config
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
		cfg:  cfg,
		font: font,
	}, nil
}

func (r renderer) refresh(ctx context.Context, dst draw.Image) {
	// TODO: the text layout here is a bit rubbish.

	// Date in top-right corner.
	writeText(dst, 420, 50, color.Black, 36, r.font, time.Now().Format("Mon _2 Jan"))

	tasks, err := TodoistTasks(ctx, r.cfg)
	if err != nil {
		// TODO: add error to screen? or some sort of simple message?
		log.Printf("Fetching Todoist tasks: %v", err)
		return
	}
	line1 := "No tasks for today!"
	switch n := len(tasks); {
	case n == 1:
		line1 = "Just one more thing to do:"
	case n == 2:
		line1 = "A couple of tasks to tick off:"
	case n <= 5:
		line1 = "A few things that need doing:"
	default:
		line1 = "Quite a bit to get done, eh?"
	}
	writeText(dst, 2, 100, color.Black, 20, r.font, line1)
	for i, task := range tasks {
		txt := "◊ " + task.Title
		if task.Assignee != "" {
			txt += " (" + task.Assignee + ")"
		}
		// TODO: red for overdue?
		// TODO: adjust size for task count?
		writeText(dst, 10, 110+28*(i+1), color.Black, 16, r.font, txt)
	}
}

func writeText(dst draw.Image, x, y int, col color.Color, fontSize float64, font *truetype.Font, text string) {
	ctx := freetype.NewContext()
	ctx.SetDst(dst)
	ctx.SetDPI(125)
	ctx.SetClip(dst.Bounds())
	ctx.SetFont(font)
	ctx.SetFontSize(fontSize)
	ctx.SetSrc(&image.Uniform{col})
	_, err := ctx.DrawString(text, freetype.Pt(x, y))
	if err != nil {
		log.Printf("Writing text: %v", err)
	}
}
