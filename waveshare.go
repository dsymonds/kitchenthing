package main

// Code specific to the Waveshare e-Paper.
//
// The references to the spec in this file mean the "7.5inch e-Paper B V2 Specification" on
// https://www.waveshare.com/wiki/7.5inch_e-Paper_HAT_(B)

import (
	"fmt"
	"image"
	"image/color"
	"log"
	"time"

	rpio "github.com/stianeikeland/go-rpio/v4"
)

func newPaper() paper {
	// I'm running in landscape, so 800 is the width.
	// The spec identifies this as the height.
	const width = 800
	const height = 480

	return paper{
		width:  width,
		height: height,

		// Pinout using BCM numbering.
		reset: rpio.Pin(17), // spec says 10?!
		dc:    rpio.Pin(25),
		cs:    rpio.Pin(8),
		busy:  rpio.Pin(24),

		bw:  newBitmap(width, height),
		red: newBitmap(width, height),
	}
}

type paper struct {
	width, height int

	reset, dc, cs, busy rpio.Pin

	bw, red bitmap
}

func (p paper) debugf(format string, args ...interface{}) {
	if *debug {
		log.Printf(format, args...)
	}
}

func (p paper) Start() error {
	if err := rpio.Open(); err != nil {
		return fmt.Errorf("opening memory range for GPIO access: %v", err)
	}

	p.debugf("paper.Init pin config")
	if err := rpio.SpiBegin(rpio.Spi0); err != nil {
		return fmt.Errorf("setting pin modes to SPI: %v", err)
	}
	p.reset.Mode(rpio.Output)
	p.dc.Mode(rpio.Output)
	p.cs.Mode(rpio.Output)
	p.busy.Mode(rpio.Input)
	return nil
}

func (p paper) Stop() {
	p.debugf("paper.Stop start")
	defer p.debugf("paper.Stop finish")

	// TODO: Turn display all white? I think that might be better for the hardware.

	p.Sleep()

	p.debugf("paper.Stop pin unconfig")
	rpio.SpiEnd(rpio.Spi0)
	rpio.Close()
}

func (p paper) Init() error {
	p.debugf("paper.Init start")
	defer p.debugf("paper.Init finish")

	p.debugf("paper.Init reset")
	p.Reset()

	// The next sequence follows one of
	//	4.2-1) BWRmode&LUTfromregister
	// or
	//	4.2-2) BWR mode & LUT from OTP
	// from the spec.

	// Configure power setting.
	p.debugf("paper.Init Power Setting (PWR)")
	p.Command(0x01)
	// VSR_E | VS_E | VG_E
	// Internal power.
	p.Data(0x07)
	// VG_LVL[2:0]==b111
	// VGH=20V, VGL=-20V
	p.Data(0x07) // TODO: fast slew rate?
	// VDH_LVL[5:0]==b111111
	// Internal VDH power selection for K/W pixel=15.0V
	p.Data(0x3f)
	// VDL_LVL[5:0]==b111111
	// Internal VDL power selection for K/W pixel=-15.0V
	p.Data(0x3f)
	// TODO: set VDHR_LVL?

	// Power on.
	p.debugf("paper.Init Power ON (PON)")
	p.Command(0x04)
	time.Sleep(100 * time.Millisecond)
	p.debugf("paper.Init wait for not busy")
	p.WaitForNotBusy()

	// Panel settings.
	p.debugf("paper.Init Panel Setting (PSR)")
	p.Command(0x00)
	// UD | SHL | SHD_N | RST_N
	// LUT from OTP, Pixel with Black/White/Red (KWR mode), Scan up, Shift right, Booster ON, No reset.
	p.Data(0x0F)

	// Resolution.
	p.debugf("paper.Init Resolution Setting (TRES)")
	p.Command(0x61)
	// HRES[9:8]==b11
	p.Data(0x03)
	// HRES[7:3]==b00100
	// HRES=0x64=100; horizontal resolution is 100*8 = 800 (active sources 0..799)
	p.Data(0x20)
	// VRES[9:8]==b01
	p.Data(0x01)
	// VRES[7:0]==b11100000
	// VRES=0x1E0=480; vertical resolution is 480 (active gates 0..479)
	p.Data(0xE0)

	// TODO: 0x15 Dual SPI Mode (DUSPI)
	// TODO: 0x60 TCON Setting (TCON)
	// TODO: 0x50 VCOM and Data interval Setting (CDI)
	// TODO: 0x65 Gate/Source Start Setting (GSST)

	p.Clear()

	return nil
}

func (p paper) Sleep() {
	p.debugf("paper.Sleep Power OFF (POF)")
	p.Command(0x02)
	p.debugf("paper.Sleep idle wait")
	p.WaitForNotBusy()
	p.debugf("paper.Sleep Deep Sleep (DSLP)")
	p.Command(0x07, 0xA5)
}

func (p paper) Reset() {
	p.reset.Write(rpio.High)
	time.Sleep(20 * time.Millisecond)
	p.reset.Write(rpio.Low)
	time.Sleep(2 * time.Millisecond)
	p.reset.Write(rpio.High)
	time.Sleep(20 * time.Millisecond)
}

func (p paper) Clear() {
	// Initialise data to all white.
	p.bw.setAll()
	p.red.clearAll()
}

func (p paper) DisplayRefresh() {
	p.debugf("paper.DisplayRefresh start")
	start := time.Now()
	defer func() {
		p.debugf("paper.DisplayRefresh finish (took %v)", time.Since(start).Truncate(time.Millisecond))
	}()

	p.debugf("paper.DisplayRefresh Data Start Transmission 1 (DTM1)")
	p.Command(0x10)
	p.Data(p.bw.bits...)

	p.debugf("paper.DisplayRefresh Data Start Transmission 2 (DTM2)")
	p.Command(0x13)
	p.Data(p.red.bits...)

	p.debugf("paper.DisplayRefresh Display Refresh (DRF)")
	p.Command(0x12)
	time.Sleep(100 * time.Millisecond) // TODO: really needed?
	p.WaitForNotBusy()
}

func (p paper) DisplayPartialRefresh(x, y, w, h int) {
	// TODO: This doesn't work. My hardware doesn't actually support partial refreshing.
	// The subset of data is transferred just fine, but the entire display is refreshed
	// as slowly as usual, instead of just the window.

	// TODO: instead of panicking, snap x down and w up.
	if x&7 != 0 {
		panic(fmt.Sprintf("x=%d isn't a multiple of 8", x))
	}
	if w&7 != 0 {
		panic(fmt.Sprintf("w=%d isn't a multiple of 8", w))
	}

	p.debugf("paper.DisplayPartialRefresh start")
	start := time.Now()
	defer func() {
		p.debugf("paper.DisplayPartialRefresh finish (took %v)", time.Since(start).Truncate(time.Millisecond))
	}()

	p.debugf("paper.DisplayPartialRefresh Partial In (PTIN)")
	p.Command(0x91)

	p.debugf("paper.DisplayPartialRefresh Partial Window (PTL)")
	p.Command(0x90)
	hrst := x / 8           // Horizontal start channel bank
	hred := (x + w - 1) / 8 // Horizontal end channel bank
	vrst := y               // Vertical start line
	vred := y + h - 1       // Vertical end line.
	p.Data(byte(hrst >> 5))
	p.Data(byte((hrst & 0x1F) << 3))
	p.Data(byte(hred >> 5))
	p.Data(byte((hred & 0x1F) << 3))
	p.Data(byte(vrst >> 8))
	p.Data(byte(vrst & 0xFF))
	p.Data(byte(vred >> 8))
	p.Data(byte(vred & 0xFF))
	p.Data(0x01)                     // PT_SCAN=1
	time.Sleep(2 * time.Millisecond) // TODO: might not be needed

	p.debugf("paper.DisplayPartialRefresh Data Start Transmission 1 (DTM1)")
	p.Command(0x10)
	for row := y; row < y+h; row++ {
		p.Data(p.bw.subrow(x, row, w)...)
	}

	p.debugf("paper.DisplayPartialRefresh Data Start Transmission 2 (DTM2)")
	p.Command(0x13)
	for row := y; row < y+h; row++ {
		p.Data(p.red.subrow(x, row, w)...)
	}

	p.debugf("paper.DisplayPartialRefresh Display Refresh (DRF)")
	p.Command(0x12)
	time.Sleep(100 * time.Millisecond) // TODO: really needed?
	p.WaitForNotBusy()

	p.debugf("paper.DisplayPartialRefresh Partial Out (PTOUT)")
	p.Command(0x92)
}

// WaitForNotBusy waits until the busy pin goes high, signaling the e-Paper is not busy.
func (p paper) WaitForNotBusy() {
	for {
		p.Command(0x71) // Get Status (FLG)
		if p.busy.Read() == rpio.High {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
}

func (p paper) Command(x byte, params ...byte) {
	p.dc.Write(rpio.Low)
	p.cs.Write(rpio.Low)
	rpio.SpiTransmit(x)
	p.cs.Write(rpio.High)

	for _, param := range params {
		p.Data(param)
	}
}

func (p paper) Data(x ...byte) {
	p.dc.Write(rpio.High)
	p.cs.Write(rpio.Low)
	rpio.SpiTransmit(x...)
	p.cs.Write(rpio.High)
}

type paperColor int

const (
	colWhite paperColor = iota
	colBlack
	colRed
)

func (pc paperColor) RGBA() color.RGBA {
	switch pc {
	case colBlack:
		return color.RGBA{A: 0xFF}
	case colRed:
		return color.RGBA{R: 0xFF, G: 0, B: 0, A: 0xFF}
	default:
		return color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	}
}

func pickColor(c color.Color) paperColor {
	// TODO: something nicer, like picking the closest one.
	r, g, b, _ := c.RGBA()
	if r == 0xffff && g == 0xffff && b == 0xffff {
		return colWhite
	} else if r == 0 && g == 0 && b == 0 {
		return colBlack
	} else if r == 0xffff && g == 0 && b == 0 {
		return colRed
	}
	return colWhite // white background default
}

// ColorModel implements image.Image.
func (p paper) ColorModel() color.Model {
	return color.ModelFunc(func(c color.Color) color.Color {
		switch pickColor(c) {
		case colBlack:
			return color.Black
		case colRed:
			return color.RGBA{R: 0xFF, G: 0, B: 0, A: 0xFF}
		default:
			return color.White
		}
	})
}

// Bounds implements image.Image.
func (p paper) Bounds() image.Rectangle {
	return image.Rectangle{
		Max: image.Point{X: p.width, Y: p.height},
	}
}

// At implements image.Image.
func (p paper) At(x, y int) color.Color {
	if p.red.get(x, y) {
		return colRed.RGBA()
	}
	if !p.bw.get(x, y) {
		return colBlack.RGBA()
	}
	return colWhite.RGBA()
}

// Set implements draw.Image.
func (p paper) Set(x, y int, c color.Color) {
	switch pickColor(c) {
	case colBlack:
		p.bw.clear(x, y)
		p.red.clear(x, y)
	case colRed:
		p.bw.set(x, y)
		p.red.set(x, y)
	default:
		// white
		p.bw.set(x, y)
		p.red.clear(x, y)
	}
}

type bitmap struct {
	bits          []byte
	width, height int
}

func newBitmap(width, height int) bitmap {
	if width&0x07 != 0 {
		panic(fmt.Sprintf("width %d is not a multiple of 8", width))
	}
	return bitmap{
		bits:   make([]byte, width*height/8),
		width:  width,
		height: height,
	}
}

func (b bitmap) clearAll() {
	for i := range b.bits {
		b.bits[i] = 0
	}
}

func (b bitmap) setAll() {
	for i := range b.bits {
		b.bits[i] = 0xFF
	}
}

func (b bitmap) clear(x, y int) {
	off := x + y*b.width
	i := off / 8             // byte index
	j := 1 << (7 - off&0x07) // bit mask
	b.bits[i] &^= byte(j)
}

func (b bitmap) get(x, y int) bool {
	off := x + y*b.width
	i := off / 8             // byte index
	j := 1 << (7 - off&0x07) // bit mask
	return b.bits[i]|byte(j) != 0
}

func (b bitmap) set(x, y int) {
	off := x + y*b.width
	i := off / 8             // byte index
	j := 1 << (7 - off&0x07) // bit mask
	b.bits[i] |= byte(j)
}

// subrow returns the subset of the bitmap starting at (x, y), going for w bits.
func (b bitmap) subrow(x, y, w int) []byte {
	off := x + y*b.width
	i := off / 8 // byte index
	return b.bits[i : i+w/8]
}
