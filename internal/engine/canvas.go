package engine

// canvas.go — Canvas 2D host side. The JavaScript API lives in
// canvas_prelude.js; every drawing operation lands in internal/raster.
//
// Handles: each VM keeps a table of integer handles (canvases, gradients,
// patterns, decoded images). The page-visible objects hold the integers.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/jpeg"
	"image/png"
	"math"
	"strings"
	"sync"

	_ "embed"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"modernc.org/quickjs"

	"github.com/sweefbrowser/sweef/internal/raster"
	"github.com/sweefbrowser/sweef/internal/render"
)

//go:embed canvas_prelude.js.z
var canvasPreludeJSZ string

const (
	cvMaxDim    = 16384
	cvMaxPixels = 128 << 20
)

func init() {
	registerPreludeZ("canvas", canvasPreludeJSZ)
	registerHostExtension("canvas", func(j *jsVM) map[string]hostFn {
		h := newCanvasHost(j)
		j.cvh = h
		return h.fns()
	})
}

type cvCanvas struct {
	mu     sync.Mutex
	id     int
	w, h   int
	ctx    *raster.Context
	opaque bool
	node   int64
}

// Bitmap implements BitmapSource: a snapshot of the current pixels.
func (c *cvCanvas) Bitmap() image.Image {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx == nil {
		return nil
	}
	return c.snapshotLocked().Image()
}

// Size implements BitmapSource.
func (c *cvCanvas) Size() (int, int) { return c.w, c.h }

func (c *cvCanvas) snapshotLocked() *raster.Surface {
	s := c.ctx.Surface().Clone()
	if c.opaque {
		for i := 0; i+3 < len(s.Pix); i += 4 {
			s.Pix[i+3] = 255
		}
	}
	return s
}

type cvPattern struct {
	img        *raster.Surface
	repX, repY bool
	m          raster.Matrix
}

type cvHost struct {
	j      *jsVM
	mu     sync.Mutex
	next   int
	ctxs   map[int]*cvCanvas
	objs   map[int]any
	imgs   map[string]*raster.Surface
	imgOrd []string
}

func newCanvasHost(j *jsVM) *cvHost {
	h := &cvHost{j: j, next: 1, ctxs: map[int]*cvCanvas{}, objs: map[int]any{}, imgs: map[string]*raster.Surface{}}
	j.cv = h
	return h
}

// imageData returns the canvas pixels of the rectangle as unpremultiplied RGBA (getImageData), or nil.
func (h *cvHost) imageData(id, x, y, w, hh int) []byte {
	c := h.get(id)
	if c == nil || c.ctx == nil || w < 0 || hh < 0 || w*hh > cvMaxPixels/4 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.ctx.GetImageData(x, y, w, hh)
	if c.opaque {
		for i := 0; i+3 < len(d); i += 4 {
			a := uint32(d[i+3])
			d[i], d[i+1], d[i+2], d[i+3] = uint8(uint32(d[i])*a/255), uint8(uint32(d[i+1])*a/255), uint8(uint32(d[i+2])*a/255), 255
		}
	}
	return d
}

// registerBufferFns installs getImageData/putImageData paths that move pixels as ArrayBuffers instead of
// strings: one memcpy instead of a per-pixel decode loop in the VM (a 1280x720 readback went from ~0.4 s to
// a few ms).
func (h *cvHost) registerBufferFns() error {
	j := h.j
	if wasmQJS() != nil {
		return nil // the raw QuickJS layout check failed: keep the string paths
	}
	// the raw handles are taken on each call, on the goroutine running the VM (libc state is per goroutine)
	if err := j.vm.RegisterFunc("__swCvGetBuf", func(id, x, y, w, hh int) quickjs.Value {
		return rawOf(j.vm).newCopyBuffer(j.vm, h.imageData(id, x, y, w, hh))
	}, false); err != nil {
		return err
	}
	return j.vm.RegisterFunc("__swCvPutBuf", func(id int, buf quickjs.Value, w, hh, dx, dy, x0, y0, w0, h0 int) bool {
		b, ok := rawOf(j.vm).bufferBytes(&buf)
		if !ok || len(b) < w*hh*4 {
			return false
		}
		_ = h.with([]any{float64(id)}, func(c *cvCanvas) { c.ctx.PutImageData(b, w, hh, dx, dy, x0, y0, w0, h0) })
		return true
	}, false)
}

func cvF(a []any, i int) float64 {
	if i >= len(a) {
		return 0
	}
	switch v := a[i].(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float64:
		return v
	case bool:
		if v {
			return 1
		}
	}
	return 0
}

func cvI(a []any, i int) int { return int(cvF(a, i)) }

func cvS(a []any, i int) string {
	if i >= len(a) || a[i] == nil {
		return ""
	}
	if s, ok := a[i].(string); ok {
		return s
	}
	return fmt.Sprint(a[i])
}

func cvArr(a []any, i int) []float64 {
	if i >= len(a) {
		return nil
	}
	if o, ok := a[i].(*quickjs.Object); ok {
		var out []float64
		if err := o.Into(&out); err != nil {
			return nil
		}
		return out
	}
	arr, ok := a[i].([]any)
	if !ok {
		return nil
	}
	out := make([]float64, len(arr))
	for k := range arr {
		out[k] = cvF(arr, k)
	}
	return out
}

func (h *cvHost) get(id int) *cvCanvas {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ctxs[id]
}

func (h *cvHost) put(o any) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	h.objs[id] = o
	return id
}

func cvPath(arr []float64) *raster.Path {
	p := raster.NewPath()
	i := 0
	for i < len(arr) {
		op := int(arr[i])
		i++
		switch op {
		case 0:
			if i+2 > len(arr) {
				return p
			}
			p.MoveTo(arr[i], arr[i+1])
			i += 2
		case 1:
			if i+2 > len(arr) {
				return p
			}
			p.LineTo(arr[i], arr[i+1])
			i += 2
		case 2:
			if i+4 > len(arr) {
				return p
			}
			p.QuadTo(arr[i], arr[i+1], arr[i+2], arr[i+3])
			i += 4
		case 3:
			if i+6 > len(arr) {
				return p
			}
			p.CubicTo(arr[i], arr[i+1], arr[i+2], arr[i+3], arr[i+4], arr[i+5])
			i += 6
		case 4:
			p.Close()
		default:
			return p
		}
	}
	return p
}

func pathToArr(p *raster.Path) []any {
	var out []any
	i := 0
	for _, c := range p.Cmds {
		switch c {
		case 'M':
			out = append(out, 0, p.Pts[i], p.Pts[i+1])
			i += 2
		case 'L':
			out = append(out, 1, p.Pts[i], p.Pts[i+1])
			i += 2
		case 'Q':
			out = append(out, 2, p.Pts[i], p.Pts[i+1], p.Pts[i+2], p.Pts[i+3])
			i += 4
		case 'C':
			out = append(out, 3, p.Pts[i], p.Pts[i+1], p.Pts[i+2], p.Pts[i+3], p.Pts[i+4], p.Pts[i+5])
			i += 6
		case 'Z':
			out = append(out, 4)
		}
	}
	return out
}

func rule(r string) raster.FillRule {
	if r == "evenodd" {
		return raster.EvenOdd
	}
	return raster.NonZero
}

func (h *cvHost) fns() map[string]hostFn {
	return map[string]hostFn{
		"cvNew": func(a []any) (any, error) {
			w, hh := cvI(a, 0), cvI(a, 1)
			if w < 0 || hh < 0 || w > cvMaxDim || hh > cvMaxDim || w*hh > cvMaxPixels {
				return 0, nil
			}
			c := &cvCanvas{w: w, h: hh, opaque: !cvB(a, 2)}
			c.alloc()
			h.mu.Lock()
			c.id = h.next
			h.next++
			h.ctxs[c.id] = c
			h.mu.Unlock()
			return c.id, nil
		},
		"cvResize": func(a []any) (any, error) {
			c := h.get(cvI(a, 0))
			if c == nil {
				return false, nil
			}
			w, hh := cvI(a, 1), cvI(a, 2)
			if w < 0 || hh < 0 || w > cvMaxDim || hh > cvMaxDim || w*hh > cvMaxPixels {
				c.mu.Lock()
				c.w, c.h, c.ctx = w, hh, nil
				c.mu.Unlock()
				return false, nil
			}
			c.mu.Lock()
			c.w, c.h = w, hh
			c.alloc()
			c.mu.Unlock()
			return true, nil
		},
		"cvBind": func(a []any) (any, error) {
			c := h.get(cvI(a, 0))
			if c == nil || h.j.tab == nil {
				return nil, nil
			}
			c.node = int64(cvI(a, 1))
			if c.node != 0 {
				h.j.tab.SetBitmapSource(c.node, c)
			}
			return nil, nil
		},
		"cvFree": func(a []any) (any, error) {
			id := cvI(a, 0)
			h.mu.Lock()
			if c := h.ctxs[id]; c != nil && c.node == 0 {
				delete(h.ctxs, id)
			}
			delete(h.objs, id)
			h.mu.Unlock()
			return nil, nil
		},
		"cvColor": func(a []any) (any, error) {
			c, ok := raster.ParseColor(cvS(a, 0), raster.Black)
			if !ok {
				return nil, nil
			}
			r, g, b, al := c.Premul8()
			_ = r
			_ = g
			_ = b
			_ = al
			return []any{c.R * 255, c.G * 255, c.B * 255, c.A, c.CSS()}, nil
		},
		"cvFont": func(a []any) (any, error) {
			spec, ok := raster.ParseFont(cvS(a, 0), cvF(a, 1))
			if !ok {
				return nil, nil
			}
			return spec.String(), nil
		},
		"cvParseSVG": func(a []any) (any, error) {
			p, ok := raster.ParseSVGPath(cvS(a, 0))
			if !ok {
				// per spec, parse up to the error: keep what was built
				if p.Empty() {
					return []any{}, nil
				}
			}
			return pathToArr(p), nil
		},
		"cvFilter": func(a []any) (any, error) {
			_, ok := raster.ParseFilterList(cvS(a, 0), raster.Black)
			return ok, nil
		},
		"cvSet":        h.set,
		"cvSave":       func(a []any) (any, error) { return nil, h.with(a, func(c *cvCanvas) { c.ctx.Save() }) },
		"cvRestore":    func(a []any) (any, error) { return nil, h.with(a, func(c *cvCanvas) { c.ctx.Restore() }) },
		"cvResetCtx":   func(a []any) (any, error) { return nil, h.with(a, func(c *cvCanvas) { c.alloc() }) },
		"cvClear":      func(a []any) (any, error) { return nil, h.with(a, func(c *cvCanvas) { c.ctx.Surface().Clear() }) },
		"cvFillPath":   func(a []any) (any, error) { return nil, h.withPath(a, false) },
		"cvStrokePath": func(a []any) (any, error) { return nil, h.withPath(a, true) },
		"cvClipPath":   h.clipPath,
		"cvFillRect": func(a []any) (any, error) {
			return nil, h.with(a, func(c *cvCanvas) { c.ctx.FillRect(cvF(a, 1), cvF(a, 2), cvF(a, 3), cvF(a, 4)) })
		},
		"cvStrokeRect": func(a []any) (any, error) {
			return nil, h.with(a, func(c *cvCanvas) { c.ctx.StrokeRect(cvF(a, 1), cvF(a, 2), cvF(a, 3), cvF(a, 4)) })
		},
		"cvClearRect": func(a []any) (any, error) {
			return nil, h.with(a, func(c *cvCanvas) { c.ctx.ClearRect(cvF(a, 1), cvF(a, 2), cvF(a, 3), cvF(a, 4)) })
		},
		"cvFillText": func(a []any) (any, error) {
			return nil, h.with(a, func(c *cvCanvas) { c.ctx.FillText(cvS(a, 1), cvF(a, 2), cvF(a, 3), cvF(a, 4)) })
		},
		"cvStrokeText": func(a []any) (any, error) {
			return nil, h.with(a, func(c *cvCanvas) { c.ctx.StrokeText(cvS(a, 1), cvF(a, 2), cvF(a, 3), cvF(a, 4)) })
		},
		"cvMeasure": func(a []any) (any, error) {
			var out []any
			err := h.with(a, func(c *cvCanvas) {
				m := c.ctx.MeasureText(cvS(a, 1))
				out = []any{m.Width, m.ActualLeft, m.ActualRight, m.ActualAscent, m.ActualDescent, m.FontAscent, m.FontDescent, m.EmAscent, m.EmDescent, m.Hanging, m.Alphabetic, m.Ideograph}
			})
			return out, err
		},
		"cvGradient": func(a []any) (any, error) {
			d := &raster.GradientDef{}
			switch cvS(a, 0) {
			case "linear":
				d.Kind, d.X0, d.Y0, d.X1, d.Y1 = raster.LinearGrad, cvF(a, 1), cvF(a, 2), cvF(a, 3), cvF(a, 4)
			case "radial":
				d.Kind, d.X0, d.Y0, d.R0, d.X1, d.Y1, d.R1 = raster.RadialGrad, cvF(a, 1), cvF(a, 2), cvF(a, 3), cvF(a, 4), cvF(a, 5), cvF(a, 6)
			default:
				d.Kind, d.R0, d.X0, d.Y0 = raster.ConicGrad, cvF(a, 1), cvF(a, 2), cvF(a, 3)
			}
			return h.put(d), nil
		},
		"cvGradStop": func(a []any) (any, error) {
			d, _ := h.objOf(cvI(a, 0)).(*raster.GradientDef)
			c, ok := raster.ParseColor(cvS(a, 2), raster.Black)
			if d == nil || !ok {
				return false, nil
			}
			d.Stops = append(d.Stops, raster.GradStop{Pos: cvF(a, 1), Color: c})
			return true, nil
		},
		"cvPattern": func(a []any) (any, error) {
			s := h.surfaceOf(cvS(a, 0), a, 1)
			if s == nil {
				return 0, nil
			}
			rep := cvS(a, 2)
			p := &cvPattern{img: s, m: raster.Identity, repX: rep == "" || rep == "repeat" || rep == "repeat-x", repY: rep == "" || rep == "repeat" || rep == "repeat-y"}
			return h.put(p), nil
		},
		"cvPatternMatrix": func(a []any) (any, error) {
			if p, _ := h.objOf(cvI(a, 0)).(*cvPattern); p != nil {
				p.m = raster.Matrix{cvF(a, 1), cvF(a, 2), cvF(a, 3), cvF(a, 4), cvF(a, 5), cvF(a, 6)}
			}
			return nil, nil
		},
		"cvImgLoad": func(a []any) (any, error) {
			s := h.loadURL(cvS(a, 0))
			if s == nil {
				return nil, nil
			}
			return []any{h.put(s), s.W(), s.H()}, nil
		},
		"cvImgDecode": func(a []any) (any, error) {
			b := unlatin1(cvS(a, 0))
			s := decodeSurface(b)
			if s == nil {
				return nil, nil
			}
			return []any{h.put(s), s.W(), s.H()}, nil
		},
		"cvImgFromData": func(a []any) (any, error) {
			b := unlatin1(cvS(a, 0))
			w, hh := cvI(a, 1), cvI(a, 2)
			if len(b) < w*hh*4 {
				return nil, nil
			}
			s := raster.NewSurface(w, hh)
			s.SetNRGBA(b, w, hh, 0, 0)
			return h.put(s), nil
		},
		"cvImgFromCtx": func(a []any) (any, error) {
			c := h.get(cvI(a, 0))
			if c == nil || c.ctx == nil {
				return nil, nil
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			return h.put(c.snapshotLocked()), nil
		},
		"cvDrawImage": h.drawImage,
		"cvGetImageData": func(a []any) (any, error) {
			c := h.get(cvI(a, 0))
			if c == nil || c.ctx == nil {
				return "", nil
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			x, y, w, hh := cvI(a, 1), cvI(a, 2), cvI(a, 3), cvI(a, 4)
			if w < 0 || hh < 0 || w*hh > cvMaxPixels/4 {
				return nil, fmt.Errorf("out of memory")
			}
			d := c.ctx.GetImageData(x, y, w, hh)
			if c.opaque {
				for i := 0; i+3 < len(d); i += 4 {
					a := uint32(d[i+3])
					d[i], d[i+1], d[i+2], d[i+3] = uint8(uint32(d[i])*a/255), uint8(uint32(d[i+1])*a/255), uint8(uint32(d[i+2])*a/255), 255
				}
			}
			return latin1(d), nil
		},
		"cvGetImageData16": func(a []any) (any, error) {
			// same pixels as cvGetImageData packed two bytes per code point
			// (value + 0x10000, always a valid non-surrogate code point), so the
			// VM decodes with half as many loop iterations
			c := h.get(cvI(a, 0))
			if c == nil || c.ctx == nil {
				return "", nil
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			x, y, w, hh := cvI(a, 1), cvI(a, 2), cvI(a, 3), cvI(a, 4)
			if w < 0 || hh < 0 || w*hh > cvMaxPixels/4 {
				return nil, fmt.Errorf("out of memory")
			}
			d := c.ctx.GetImageData(x, y, w, hh)
			if c.opaque {
				for i := 0; i+3 < len(d); i += 4 {
					a := uint32(d[i+3])
					d[i], d[i+1], d[i+2], d[i+3] = uint8(uint32(d[i])*a/255), uint8(uint32(d[i+1])*a/255), uint8(uint32(d[i+2])*a/255), 255
				}
			}
			var sb strings.Builder
			sb.Grow(len(d) * 2)
			for i := 0; i+1 < len(d); i += 2 {
				sb.WriteRune(rune(uint32(d[i])|uint32(d[i+1])<<8) + 0x10000)
			}
			return sb.String(), nil
		},
		"cvPutImageData": func(a []any) (any, error) {
			b := unlatin1(cvS(a, 1))
			w, hh := cvI(a, 2), cvI(a, 3)
			if len(b) < w*hh*4 {
				return nil, fmt.Errorf("bad image data")
			}
			return nil, h.with(a, func(c *cvCanvas) {
				c.ctx.PutImageData(b, w, hh, cvI(a, 4), cvI(a, 5), cvI(a, 6), cvI(a, 7), cvI(a, 8), cvI(a, 9))
			})
		},
		"cvPointInPath": func(a []any) (any, error) {
			var res bool
			err := h.with(a, func(c *cvCanvas) {
				p := cvPath(cvArr(a, 1))
				x, y := cvF(a, 2), cvF(a, 3)
				if cvB(a, 5) { // device space path
					res = raster.PointInPath(p, raster.Identity, x, y, rule(cvS(a, 4)))
				} else {
					res = c.ctx.IsPointInPath(p, x, y, rule(cvS(a, 4)))
				}
			})
			return res, err
		},
		"cvPointInStroke": func(a []any) (any, error) {
			var res bool
			err := h.with(a, func(c *cvCanvas) {
				p := cvPath(cvArr(a, 1))
				if cvB(a, 4) {
					if inv, ok := c.ctx.Transform().Invert(); ok {
						p = p.Transform(inv)
					}
				}
				res = c.ctx.IsPointInStroke(p, cvF(a, 2), cvF(a, 3))
			})
			return res, err
		},
		"cvEncodeNode": func(a []any) (any, error) {
			s := h.surfaceOf("node", a, 0)
			if s == nil {
				return "", nil
			}
			return encodeSurface(s, cvS(a, 1), cvF(a, 2), cvB(a, 3))
		},
		"cvEncode": func(a []any) (any, error) {
			c := h.get(cvI(a, 0))
			if c == nil || c.ctx == nil || c.w == 0 || c.h == 0 {
				return "", nil
			}
			c.mu.Lock()
			s := c.snapshotLocked()
			c.mu.Unlock()
			return encodeSurface(s, cvS(a, 1), cvF(a, 2), cvB(a, 3))
		},
	}
}

func cvB(a []any, i int) bool {
	if i >= len(a) {
		return false
	}
	switch v := a[i].(type) {
	case bool:
		return v
	case nil:
		return false
	case string:
		return v != ""
	}
	return cvF(a, i) != 0
}

func (c *cvCanvas) alloc() {
	if c.w <= 0 || c.h <= 0 {
		c.ctx = raster.NewContext(0, 0)
		return
	}
	s := raster.NewSurface(c.w, c.h)
	c.ctx = raster.NewContextSurface(s)
}

func (h *cvHost) objOf(id int) any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.objs[id]
}

func (h *cvHost) with(a []any, fn func(c *cvCanvas)) error {
	c := h.get(cvI(a, 0))
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx == nil {
		return nil
	}
	fn(c)
	return nil
}

func (h *cvHost) brush(c *cvCanvas, which string, a []any, i int) {
	// handled in set
}

func (h *cvHost) set(a []any) (any, error) {
	var res any
	err := h.with(a, func(c *cvCanvas) {
		x := c.ctx
		key := cvS(a, 1)
		switch key {
		case "fillColor", "strokeColor":
			col := raster.Color{R: cvF(a, 2) / 255, G: cvF(a, 3) / 255, B: cvF(a, 4) / 255, A: cvF(a, 5)}
			if key == "fillColor" {
				x.SetFill(raster.Brush{Color: col})
			} else {
				x.SetStroke(raster.Brush{Color: col})
			}
		case "fillObj", "strokeObj":
			var b raster.Brush
			switch o := h.objOf(cvI(a, 2)).(type) {
			case *raster.GradientDef:
				cp := *o
				cp.Stops = append([]raster.GradStop(nil), o.Stops...)
				b.Gradient = &cp
			case *cvPattern:
				b.Pattern = &raster.Pattern{Img: o.img, RepX: o.repX, RepY: o.repY, M: o.m}
			default:
				return
			}
			if key == "fillObj" {
				x.SetFill(b)
			} else {
				x.SetStroke(b)
			}
		case "transform":
			x.SetTransform(raster.Matrix{cvF(a, 2), cvF(a, 3), cvF(a, 4), cvF(a, 5), cvF(a, 6), cvF(a, 7)})
		case "line":
			ss := x.StrokeStyle()
			ss.Width = cvF(a, 2)
			ss.Cap = raster.LineCap(cvI(a, 3))
			ss.Join = raster.LineJoin(cvI(a, 4))
			ss.MiterLimit = cvF(a, 5)
			ss.DashOffset = cvF(a, 7)
			ss.Dash = cvArr(a, 6)
			x.SetStrokeStyle(ss)
		case "globalAlpha":
			x.SetGlobalAlpha(cvF(a, 2))
		case "gco":
			if op, ok := raster.ParseOp(cvS(a, 2)); ok {
				x.SetOp(op)
				res = true
			} else {
				res = false
			}
		case "smoothing":
			x.SetSmoothing(cvB(a, 2), raster.Quality(cvI(a, 3)))
		case "shadow":
			x.SetShadow(raster.Color{R: cvF(a, 2) / 255, G: cvF(a, 3) / 255, B: cvF(a, 4) / 255, A: cvF(a, 5)}, cvF(a, 6), cvF(a, 7), cvF(a, 8))
		case "filter":
			fl, ok := raster.ParseFilterList(cvS(a, 2), raster.Black)
			if ok {
				x.SetFilters(fl)
			}
			res = ok
		case "font":
			res = x.SetFont(cvS(a, 2))
			if res == true {
				h.scopeCanvasFont(x)
			}
		case "textAlign":
			x.SetTextAlign(cvS(a, 2))
		case "textBaseline":
			x.SetTextBaseline(cvS(a, 2))
		case "direction":
			x.SetDirection(cvS(a, 2))
		case "letterSpacing":
			x.SetLetterSpacing(cvF(a, 2))
		case "wordSpacing":
			x.SetWordSpacing(cvF(a, 2))
		case "fontKerning":
			x.SetFontKerning(cvS(a, 2))
		}
	})
	return res, err
}

func (h *cvHost) withPath(a []any, stroke bool) error {
	// args: id, path, rule, deviceSpace
	return h.with(a, func(c *cvCanvas) {
		p := cvPath(cvArr(a, 1))
		dev := cvB(a, 3)
		if stroke {
			if dev {
				inv, ok := c.ctx.Transform().Invert()
				if !ok {
					return
				}
				p = p.Transform(inv)
			}
			c.ctx.StrokePath(p)
			return
		}
		if dev {
			c.ctx.FillPathDevice(p, rule(cvS(a, 2)))
		} else {
			c.ctx.FillPath(p, rule(cvS(a, 2)))
		}
	})
}

func (h *cvHost) clipPath(a []any) (any, error) {
	return nil, h.with(a, func(c *cvCanvas) {
		p := cvPath(cvArr(a, 1))
		if cvB(a, 3) {
			c.ctx.ClipPathDevice(p, rule(cvS(a, 2)))
		} else {
			c.ctx.ClipPath(p, rule(cvS(a, 2)))
		}
	})
}

// surfaceOf resolves an image source (kind, ref) to a surface.
func (h *cvHost) surfaceOf(kind string, a []any, i int) *raster.Surface {
	switch kind {
	case "ctx":
		c := h.get(cvI(a, i))
		if c == nil || c.ctx == nil || c.w == 0 || c.h == 0 {
			return nil
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.snapshotLocked()
	case "img":
		s, _ := h.objOf(cvI(a, i)).(*raster.Surface)
		return s
	case "url":
		return h.loadURL(cvS(a, i))
	case "node":
		t := h.j.tab
		if t == nil || t.dom == nil {
			return nil
		}
		n := t.dom.NodeByID(int64(cvI(a, i)))
		if n == nil {
			return nil
		}
		src := t.BitmapSourceFor(n)
		if src == nil {
			return nil
		}
		img := src.Bitmap()
		if img == nil {
			return nil
		}
		return raster.FromImage(img)
	}
	return nil
}

func (h *cvHost) loadURL(u string) *raster.Surface {
	t := h.j.tab
	if u == "" || t == nil {
		return nil
	}
	abs, err := resolveURL(t.URL(), u)
	if err != nil {
		return nil
	}
	h.mu.Lock()
	if s, ok := h.imgs[abs]; ok {
		h.mu.Unlock()
		return s
	}
	h.mu.Unlock()
	var data []byte
	if strings.HasPrefix(abs, "data:") {
		_, d, err := decodeDataURL(abs)
		if err != nil {
			return nil
		}
		data = d
	} else {
		body, status, err := t.fetchResourceKind(abs, KindImage)
		if err != nil || status >= 400 {
			return nil
		}
		data = []byte(body)
	}
	s := decodeSurface(data)
	if s == nil {
		return nil
	}
	h.mu.Lock()
	if len(h.imgOrd) > 64 {
		delete(h.imgs, h.imgOrd[0])
		h.imgOrd = h.imgOrd[1:]
	}
	h.imgs[abs] = s
	h.imgOrd = append(h.imgOrd, abs)
	h.mu.Unlock()
	return s
}

// drawImage: id, kind, ref, nargs(3|5|9), n0..n7
func (h *cvHost) drawImage(a []any) (any, error) {
	kind := cvS(a, 1)
	src := h.surfaceOf(kind, a, 2)
	if src == nil {
		return false, nil
	}
	n := cvI(a, 3)
	v := func(i int) float64 { return cvF(a, 4+i) }
	iw, ih := float64(src.W()), float64(src.H())
	var sx, sy, sw, sh, dx, dy, dw, dh float64
	switch n {
	case 3:
		sx, sy, sw, sh = 0, 0, iw, ih
		dx, dy, dw, dh = v(0), v(1), iw, ih
	case 5:
		sx, sy, sw, sh = 0, 0, iw, ih
		dx, dy, dw, dh = v(0), v(1), v(2), v(3)
	default:
		sx, sy, sw, sh = v(0), v(1), v(2), v(3)
		dx, dy, dw, dh = v(4), v(5), v(6), v(7)
	}
	if sw == 0 || sh == 0 || dw == 0 || dh == 0 {
		return true, nil
	}
	// normalise negative extents
	if sw < 0 {
		sx, sw = sx+sw, -sw
	}
	if sh < 0 {
		sy, sh = sy+sh, -sh
	}
	if dw < 0 {
		dx, dw = dx+dw, -dw
	}
	if dh < 0 {
		dy, dh = dy+dh, -dh
	}
	// clip the source to the image, scaling the destination to match
	cx0, cy0 := math.Max(sx, 0), math.Max(sy, 0)
	cx1, cy1 := math.Min(sx+sw, iw), math.Min(sy+sh, ih)
	if cx1 <= cx0 || cy1 <= cy0 {
		return true, nil
	}
	fx, fy := dw/sw, dh/sh
	dx += (cx0 - sx) * fx
	dy += (cy0 - sy) * fy
	dw = (cx1 - cx0) * fx
	dh = (cy1 - cy0) * fy
	sx, sy, sw, sh = cx0, cy0, cx1-cx0, cy1-cy0
	err := h.with(a, func(c *cvCanvas) { c.ctx.DrawImage(src, sx, sy, sw, sh, dx, dy, dw, dh) })
	return true, err
}

func encodeSurface(s *raster.Surface, mime string, quality float64, bin bool) (any, error) {
	var buf bytes.Buffer
	nrgba := s.ToNRGBA()
	switch mime {
	case "image/jpeg":
		// composite over black like browsers do
		rgba := image.NewRGBA(nrgba.Rect)
		for i := 0; i+3 < len(nrgba.Pix); i += 4 {
			a := uint32(nrgba.Pix[i+3])
			rgba.Pix[i] = uint8(uint32(nrgba.Pix[i]) * a / 255)
			rgba.Pix[i+1] = uint8(uint32(nrgba.Pix[i+1]) * a / 255)
			rgba.Pix[i+2] = uint8(uint32(nrgba.Pix[i+2]) * a / 255)
			rgba.Pix[i+3] = 255
		}
		q := 92
		if quality >= 0 && quality <= 1 && !math.IsNaN(quality) && quality != 0.92 {
			q = int(math.Round(quality * 100))
		}
		if err := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: q}); err != nil {
			return nil, err
		}
	default:
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := enc.Encode(&buf, nrgba); err != nil {
			return nil, err
		}
	}
	if bin {
		return latin1(buf.Bytes()), nil
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// latin1 maps each byte to the code point of the same value so binary data
// crosses the host boundary as a plain string (about 15x faster than base64
// through atob in the VM).
func latin1(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b) + len(b)/2)
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

func unlatin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		out = append(out, byte(r))
	}
	return out
}

var _ = color.NRGBA{}

// decodeSurface decodes raster image bytes, or rasterizes SVG markup at its
// intrinsic size (300x150 when the SVG declares none).
func decodeSurface(b []byte) *raster.Surface {
	if img, _, err := image.Decode(bytes.NewReader(b)); err == nil {
		return raster.FromImage(img)
	}
	head := b
	if len(head) > 2048 {
		head = head[:2048]
	}
	if !bytes.Contains(bytes.ToLower(head), []byte("<svg")) {
		return nil
	}
	doc := render.ParseSVG(string(b))
	if doc == nil {
		return nil
	}
	w, h := doc.IntrinsicSize()
	if w <= 0 {
		w = 300
	}
	if h <= 0 {
		h = 150
	}
	return raster.RenderSVG(doc, int(math.Ceil(w)), int(math.Ceil(h)), render.Color{A: 1})
}

// putRGBA stores straight-alpha RGBA pixels as an image object (the "img" source of cvDrawImage) and returns its handle.
func (h *cvHost) putRGBA(rgba []byte, w, hh int) int {
	s := raster.NewSurface(w, hh)
	s.SetNRGBA(rgba, w, hh, 0, 0)
	return h.put(s)
}
