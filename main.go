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
	"strings"
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
	httpFlag   = flag.String("http", "localhost:8080", "`address` on which to serve HTTP")

	testRender  = flag.String("test_render", "", "`filename` to render a PNG to")
	testTodoist = flag.Bool("test_todoist", false, "whether to use fake Todoist data")
)

type Config struct {
	Font            string        `yaml:"font"`
	RefreshPeriod   time.Duration `yaml:"refresh_period"`
	TodoistAPIToken string        `yaml:"todoist_api_token"`
	PhotosDir       string        `yaml:"photos_dir"`
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
	ref := newRefresher(cfg)

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

	s := &server{
		startTime: time.Now(),
	}
	http.Handle("/", s)
	log.SetOutput(io.MultiWriter(os.Stderr, s))

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

	if err := p.Start(); err != nil {
		log.Fatalf("Paper start: %v", err)
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
		if err := loop(ctx, cfg, rend, ref, p); err != nil {
			log.Printf("Loop failed: %v", err)
		}
		cancel()
	}()

	// Wait until interrupted or something else causes a graceful shutdown.
exit:
	<-ctx.Done()
	wg.Wait()
	p.Stop()
	log.Printf("kitchenthing done")
}

type server struct {
	startTime time.Time

	mu     sync.Mutex
	logBuf bytes.Buffer
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
			b = b[i:]
		}
		copy(s.logBuf.Bytes(), b)
		s.logBuf.Truncate(len(b))
	}

	return
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := struct {
		Uptime time.Duration
		Logs   string
	}{
		Uptime: time.Since(s.startTime).Truncate(time.Minute),
	}

	s.mu.Lock()
	data.Logs = s.logBuf.String()
	s.mu.Unlock()

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

func loop(ctx context.Context, cfg Config, rend renderer, ref *refresher, p paper) error {
	var prev displayData
	for {
		data := ref.Refresh(ctx)

		if !data.Equal(prev) {
			log.Printf("New data to be displayed; refreshing now")
			p.Init()
			rend.Render(p, data)
			p.DisplayRefresh()
			p.Sleep()
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

	photosDir string
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

		photosDir: cfg.PhotosDir,
	}, nil
}

type refresher struct {
	cfg Config
	ts  *TodoistSyncer
}

func newRefresher(cfg Config) *refresher {
	return &refresher{
		cfg: cfg,
		ts:  NewTodoistSyncer(cfg),
	}
}

type displayData struct {
	today time.Time // only day resolution

	tasks []renderableTask

	// TODO: report errors?
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
			{4, t0, "something really important", "David", "House"},
			{3, tset, "something important", "", "House"},
			{2, t0, "something nice to do", "", "Other"},
			{1, t0, "if there's time", "", "Other"},
		}
		return dd
	}

	if err := r.ts.Sync(ctx); err != nil {
		// TODO: add error to screen? or some sort of simple message?
		log.Printf("Syncing from Todoist: %v", err)
	} else {
		dd.tasks = r.ts.RenderableTasks()
	}

	return dd
}

func (r renderer) Render(dst draw.Image, data displayData) {
	// Date in top-right corner.
	dateBL := r.writeText(dst, image.Pt(-2, 2), topLeft, color.Black, r.xlarge, data.today.Format("Mon 2 Jan"))

	var line1 string
	switch n := len(data.tasks); {
	case n == 0:
		line1 = "All done for today!"
	case n == 1:
		line1 = "Just one more thing:"
	case n == 2:
		line1 = "A couple of tasks:"
	case n < 5:
		line1 = "A few things to do:"
	default:
		line1 = "Quite a bit, eh?"
	}
	next := image.Pt(10, dateBL.Y)
	r.writeText(dst, next, bottomLeft, color.Black, r.large, line1)
	next = image.Pt(2, dateBL.Y)

	listVPitch := r.normal.Metrics().Height.Ceil()
	listBase := image.Pt(10, next.Y+2+listVPitch) // baseline of each list entry
	for i, task := range data.tasks {             // TODO: adjust font size for task count?
		txt := fmt.Sprintf("[P%d] %s", 4-task.Priority, task.Title)
		if !task.Time.IsZero() {
			txt += " <" + task.Time.Format(time.Kitchen) + ">"
		}
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
	bottomOfListY := listBase.Y + (len(data.tasks)-1)*listVPitch

	// TODO: Find something more interesting to squeeze in?
	next = r.writeText(dst, image.Pt(-2, -2), bottomLeft, color.Black, r.tiny, "π")
	topOfFooterY := dst.Bounds().Max.Y // or use next.Y-8 if there's a substantial footer

	sub := clippedImage{
		img: dst,
		bounds: image.Rectangle{
			Min: image.Pt(10, bottomOfListY+10),
			Max: image.Pt(dst.Bounds().Max.X-10, topOfFooterY-2),
		},
	}
	if !sub.bounds.Empty() && r.photosDir != "" {
		if err := drawRandomPhoto(sub, r.photosDir); err != nil {
			log.Printf("Drawing random photo: %v", err)
		}
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
	// TODO: It'd be nice to log a message if the text busts the bounds of dst.

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

func drawRandomPhoto(dst draw.Image, dir string) error {
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("os.UserHomeDir: %w", err)
		}
		dir = filepath.Join(home, dir[2:])
	}

	opts, err := filepath.Glob(filepath.Join(dir, "*.jpg"))
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
