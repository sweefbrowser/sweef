package engine

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"

	fnet "github.com/sweefbrowser/sweef/internal/net"
)

// PNGScreenshot renders the page's flow layout (see net/layout.go) to PNG:
// text in a bitmap font at heading sizes, links in blue, form controls as
// outlined boxes with their values, images as labelled placeholders. It is a
// schematic of the page, not a pixel replica, but its geometry is exactly the
// geometry used for element boxes and coordinate clicks, so an agent can
// point at what it sees. width/height <= 0 use the viewport; dsf scales.
func (t *Tab) PNGScreenshot(width, height int, dsf float64) []byte {
	return t.renderPNG(width, height, dsf, false)
}

// PNGScreenshotFull renders the whole page height (Playwright fullPage).
func (t *Tab) PNGScreenshotFull(width int, dsf float64) []byte {
	return t.renderPNG(width, 0, dsf, true)
}

var (
	colInk    = color.RGBA{24, 28, 36, 255}
	colMuted  = color.RGBA{110, 118, 130, 255}
	colLink   = color.RGBA{29, 78, 216, 255}
	colBorder = color.RGBA{160, 168, 180, 255}
	colFill   = color.RGBA{236, 239, 243, 255}
	colAccent = color.RGBA{37, 99, 235, 255}
	colPaper  = color.RGBA{255, 255, 255, 255}
)

func (t *Tab) renderPNG(width, height int, dsf float64, full bool) []byte {
	t.wakeJS()
	t.mu.RLock()
	dom := t.dom
	if width <= 0 {
		width = t.vpW
	}
	if height <= 0 && !full {
		height = t.vpH
	}
	if dsf <= 0 {
		dsf = t.dsf
	}
	t.mu.RUnlock()
	if width <= 0 {
		width = 1280
	}
	if dsf <= 0 {
		dsf = 1
	}
	if width > 0 && dsf > 0 {
		if rgba, ok := t.paintPage(width, height, dsf, full); ok {
			var buf bytes.Buffer
			_ = encodePNGFast(&buf, rgba)
			return buf.Bytes()
		}
	}
	var l *fnet.Layout
	if dom != nil {
		l = dom.Layout(width)
	}
	if full {
		height = 200
		if l != nil && l.Height > height {
			height = l.Height
		}
		if height > 16000 {
			height = 16000
		}
	}
	if height <= 0 {
		height = 720
	}
	sc := int(dsf + 0.5)
	if sc < 1 {
		sc = 1
	}
	if sc > 4 {
		sc = 4
	}
	img := image.NewRGBA(image.Rect(0, 0, width*sc, height*sc))
	draw.Draw(img, img.Bounds(), &image.Uniform{colPaper}, image.Point{}, draw.Src)
	p := &painter{img: img, sc: sc, w: width, h: height}
	if l != nil {
		for _, it := range l.Items {
			if it.Y > height {
				continue
			}
			p.item(it)
		}
	}
	var buf bytes.Buffer
	_ = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img)
	return buf.Bytes()
}

type painter struct {
	img  *image.RGBA
	sc   int
	w, h int
}

func (p *painter) fill(x, y, w, h int, c color.Color) {
	r := image.Rect(x*p.sc, y*p.sc, (x+w)*p.sc, (y+h)*p.sc).Intersect(p.img.Bounds())
	draw.Draw(p.img, r, &image.Uniform{c}, image.Point{}, draw.Src)
}

func (p *painter) rect(x, y, w, h int, c color.Color) {
	p.fill(x, y, w, 1, c)
	p.fill(x, y+h-1, w, 1, c)
	p.fill(x, y, 1, h, c)
	p.fill(x+w-1, y, 1, h, c)
}

// text draws s with glyph scale gs (cell 6x8 per scale unit), clipped to maxW.
func (p *painter) text(x, y int, s string, gs int, c color.Color, bold bool, maxW int) {
	if gs < 1 {
		gs = 1
	}
	adv := 6 * gs
	for _, r := range s {
		if maxW > 0 && x+adv > maxW {
			break
		}
		gl, ok := glyph5x7[r]
		if !ok {
			if r > 127 {
				gl = glyph5x7['?']
			} else {
				x += adv
				continue
			}
		}
		for ry := 0; ry < 7; ry++ {
			bits := gl[ry]
			for rx := 0; rx < 5; rx++ {
				if bits&(1<<(4-rx)) != 0 {
					p.fill(x+rx*gs, y+ry*gs, gs, gs, c)
					if bold {
						p.fill(x+rx*gs+1, y+ry*gs, gs, gs, c)
					}
				}
			}
		}
		x += adv
	}
}

func (p *painter) item(it fnet.WireItem) {
	gs := it.Scale
	if gs < 1 {
		gs = 2
	}
	pad := 2 + gs
	switch it.Kind {
	case "text":
		p.text(it.X, it.Y+2, it.Text, gs, colInk, it.Bold, p.w)
	case "heading":
		p.text(it.X, it.Y+2, it.Text, gs, colInk, true, p.w)
	case "link":
		p.text(it.X, it.Y+2, it.Text, gs, colLink, it.Bold, p.w)
		p.fill(it.X, it.Y+2+8*gs, min(it.W, p.w-it.X), 1, colLink)
	case "button":
		p.fill(it.X, it.Y, it.W, it.H, colFill)
		p.rect(it.X, it.Y, it.W, it.H, colBorder)
		p.text(it.X+8, it.Y+5, it.Text, gs, colInk, false, it.X+it.W-4)
	case "input", "select":
		p.rect(it.X, it.Y, it.W, it.H, colBorder)
		c := colInk
		p.text(it.X+pad+2, it.Y+5, it.Text, gs, c, false, it.X+it.W-4-boolInt(it.Kind == "select")*14)
		if it.Kind == "select" {
			// a small caret
			cx, cy := it.X+it.W-12, it.Y+it.H/2-1
			for i := 0; i < 4; i++ {
				p.fill(cx+i, cy+i, 8-2*i, 1, colMuted)
			}
		}
	case "checkbox", "radio":
		p.rect(it.X, it.Y, it.W, it.H, colBorder)
		if it.Filled {
			p.fill(it.X+3, it.Y+3, it.W-6, it.H-6, colAccent)
		}
	case "img":
		p.fill(it.X, it.Y, it.W, it.H, colFill)
		p.rect(it.X, it.Y, it.W, it.H, colBorder)
		label := it.Text
		if label == "" {
			label = "img"
		}
		p.text(it.X+4, it.Y+4, label, 1, colMuted, false, it.X+it.W-2)
	case "hr":
		p.fill(it.X, it.Y, it.W, 1, colBorder)
	case "marker":
		p.fill(it.X, it.Y, it.W, it.H, colInk)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
