package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
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

	rand.Seed(time.Now().UnixNano())

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
		img := image.NewPaletted(image.Rect(0, 0, 800, 480), staticPalette)
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
	bottomOfListY := listBase.Y + (len(info.tasks)-1)*listVPitch

	next = r.writeText(dst, image.Pt(-2, -2), bottomLeft, color.Black, r.tiny, "Next update: ~"+info.nextRefresh.Format("15:04:05"))
	topOfFooterY := next.Y

	sub := clippedImage{
		img: dst,
		bounds: image.Rectangle{
			Min: image.Pt(10, bottomOfListY+10),
			Max: image.Pt(dst.Bounds().Max.X-10, topOfFooterY-10),
		},
	}
	if err := drawRandomPhoto(sub); err != nil {
		log.Printf("Drawing random photo: %v", err)
	}
}

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

	if anchor == bottomLeft {
		d.Dot.Y -= boundsHeight
	}

	return image.Pt(d.Dot.X.Round(), d.Dot.Y.Round())
}

func drawRandomPhoto(dst draw.Image) error {
	opts, err := filepath.Glob("photos/*")
	if err != nil {
		return fmt.Errorf("globbing photos dir: %w", err)
	}
	if len(opts) == 0 {
		return fmt.Errorf("no files in photos dir")
	}
	filename := opts[rand.Intn(len(opts))]
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
