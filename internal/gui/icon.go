package gui

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// The FORGE mark, drawn rather than shipped.
//
// Binary image assets in a source tree need a build step to produce and cannot
// be reviewed in a diff. The mark is a rounded square and three strokes, so it
// is cheaper to describe it as geometry and rasterise on demand — and it comes
// out crisp at any size the platform asks for, instead of at the three sizes
// somebody remembered to export.

// Mark geometry, in a 32x32 design space matching the favicon in index.html.
const (
	markBox    = 32.0
	markRadius = 7.0
	markStroke = 2.6
)

var (
	markInk   = color.NRGBA{0x0a, 0x39, 0x10, 0xff} // on-primary
	markPaint = color.NRGBA{0xcb, 0xff, 0xc3, 0xff} // primary
)

// segment is one stroke of the glyph: a capsule from (x1,y1) to (x2,y2).
type segment struct{ x1, y1, x2, y2 float64 }

// The glyph is an F: a spine, an arm across the top, a shorter one at the
// middle. Same path as the favicon so the tab, the installer, and the desktop
// shortcut are unmistakably the same product.
var markStrokes = []segment{
	{10, 10, 10, 22}, // spine
	{10, 10, 21, 10}, // top arm
	{10, 16, 18, 16}, // middle arm
}

// RenderIcon draws the mark at size x size pixels.
//
// padding insets the glyph as a fraction of the canvas. Maskable icons are
// cropped to a circle by the platform, so their content has to sit inside the
// middle 80% or the launcher shaves the corners off the mark.
func RenderIcon(size int, maskable bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(size) / markBox

	// 3x3 supersampling. Cheap, and the alternative is visible stair-stepping
	// on the rounded corners at 32px where it is most obvious.
	const ss = 3
	inset, glyphScale := 0.0, 1.0
	if maskable {
		// Fill the whole canvas with paint and shrink the glyph into the
		// safe zone, which is what the spec asks for.
		glyphScale = 0.62
	}

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var inBG, inInk int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					// Sample at sub-pixel centres, in design-space units.
					px := (float64(x) + (float64(sx)+0.5)/ss) / scale
					py := (float64(y) + (float64(sy)+0.5)/ss) / scale

					if maskable {
						inBG++
					} else if insideRoundRect(px, py, inset, markBox-inset, markRadius) {
						inBG++
					}
					if insideGlyph(px, py, glyphScale) {
						inInk++
					}
				}
			}
			const n = ss * ss
			if inBG == 0 && inInk == 0 {
				continue
			}
			// Composite ink over paint over transparent, weighting each by its
			// coverage so edges blend instead of jagging.
			bg := float64(inBG) / n
			ink := float64(inInk) / n
			if ink > bg {
				ink = bg // the glyph never spills past the tile
			}
			img.SetNRGBA(x, y, blend(markPaint, markInk, bg, ink))
		}
	}
	return img
}

// blend composites the ink colour over the paint colour at the given coverages
// and returns a straight-alpha pixel.
func blend(paint, ink color.NRGBA, bgCov, inkCov float64) color.NRGBA {
	if bgCov <= 0 {
		return color.NRGBA{}
	}
	// Colour is the paint/ink mix by their relative coverage; alpha is the
	// tile's coverage.
	t := inkCov / bgCov
	mix := func(a, b uint8) uint8 {
		return uint8(math.Round(float64(a)*(1-t) + float64(b)*t))
	}
	return color.NRGBA{
		R: mix(paint.R, ink.R),
		G: mix(paint.G, ink.G),
		B: mix(paint.B, ink.B),
		A: uint8(math.Round(bgCov * 255)),
	}
}

// insideRoundRect reports whether a point falls inside a rounded square.
func insideRoundRect(px, py, min, max, r float64) bool {
	if px < min || py < min || px > max || py > max {
		return false
	}
	// Only the four corner quadrants need the radius test.
	cx, cy := px, py
	switch {
	case px < min+r:
		cx = min + r
	case px > max-r:
		cx = max - r
	default:
		return true
	}
	switch {
	case py < min+r:
		cy = min + r
	case py > max-r:
		cy = max - r
	default:
		return true
	}
	return math.Hypot(px-cx, py-cy) <= r
}

// insideGlyph reports whether a point falls inside any stroke of the mark.
// scale shrinks the glyph about the tile's centre for maskable icons.
func insideGlyph(px, py, scale float64) bool {
	if scale != 1 {
		c := markBox / 2
		px = c + (px-c)/scale
		py = c + (py-c)/scale
	}
	half := markStroke / 2
	for _, s := range markStrokes {
		if distToSegment(px, py, s) <= half {
			return true
		}
	}
	return false
}

func distToSegment(px, py float64, s segment) float64 {
	dx, dy := s.x2-s.x1, s.y2-s.y1
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return math.Hypot(px-s.x1, py-s.y1)
	}
	t := ((px-s.x1)*dx + (py-s.y1)*dy) / l2
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(s.x1+t*dx), py-(s.y1+t*dy))
}

// ---------- encoded forms ----------

var iconCache sync.Map // string -> []byte

// IconPNG returns the mark as a PNG, memoised per size.
func IconPNG(size int, maskable bool) ([]byte, error) {
	key := "png"
	if maskable {
		key = "png-mask"
	}
	key += string(rune(size))
	if v, ok := iconCache.Load(key); ok {
		return v.([]byte), nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, RenderIcon(size, maskable)); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	iconCache.Store(key, b)
	return b, nil
}

// IconICO packs several PNG sizes into a Windows .ico.
//
// Vista and later accept PNG-compressed entries directly, so no BMP encoder is
// needed — the container is a 6-byte header plus a 16-byte directory entry per
// image, then the PNG bytes.
func IconICO(sizes ...int) ([]byte, error) {
	if len(sizes) == 0 {
		sizes = []int{16, 32, 48, 64, 128, 256}
	}
	var body bytes.Buffer
	type entry struct {
		size, offset, length int
	}
	entries := make([]entry, 0, len(sizes))

	// Directory sits between the header and the image data, so offsets are
	// known only after the directory's size is fixed.
	headerLen := 6 + 16*len(sizes)
	for _, s := range sizes {
		p, err := IconPNG(s, false)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{size: s, offset: headerLen + body.Len(), length: len(p)})
		body.Write(p)
	}

	var out bytes.Buffer
	w16 := func(v uint16) { _ = binary.Write(&out, binary.LittleEndian, v) }
	w32 := func(v uint32) { _ = binary.Write(&out, binary.LittleEndian, v) }

	w16(0) // reserved
	w16(1) // type: icon
	w16(uint16(len(entries)))
	for _, e := range entries {
		dim := byte(e.size)
		if e.size >= 256 {
			dim = 0 // 0 means 256 in this format
		}
		out.WriteByte(dim) // width
		out.WriteByte(dim) // height
		out.WriteByte(0)   // palette size, 0 for truecolour
		out.WriteByte(0)   // reserved
		w16(1)             // colour planes
		w16(32)            // bits per pixel
		w32(uint32(e.length))
		w32(uint32(e.offset))
	}
	out.Write(body.Bytes())
	return out.Bytes(), nil
}

// IconICNS packs PNGs into a macOS .icns.
//
// Like .ico, modern readers accept PNG payloads, so each entry is a four-byte
// OSType, a big-endian length, and the PNG.
func IconICNS() ([]byte, error) {
	// OSTypes for the PNG-capable retina and standard sizes.
	types := []struct {
		code string
		size int
	}{
		{"icp4", 16}, {"icp5", 32}, {"icp6", 64},
		{"ic07", 128}, {"ic08", 256}, {"ic09", 512}, {"ic10", 1024},
	}
	var body bytes.Buffer
	for _, t := range types {
		p, err := IconPNG(t.size, false)
		if err != nil {
			return nil, err
		}
		body.WriteString(t.code)
		_ = binary.Write(&body, binary.BigEndian, uint32(8+len(p)))
		body.Write(p)
	}
	var out bytes.Buffer
	out.WriteString("icns")
	_ = binary.Write(&out, binary.BigEndian, uint32(8+body.Len()))
	out.Write(body.Bytes())
	return out.Bytes(), nil
}
