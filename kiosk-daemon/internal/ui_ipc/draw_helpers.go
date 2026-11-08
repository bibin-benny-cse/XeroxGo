package ui_ipc

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed assets/Inter-Regular.ttf
var fontRegularBytes []byte

//go:embed assets/Inter-Bold.ttf
var fontBoldBytes []byte

//go:embed assets/logo.png
var logoPNGBytes []byte

var (
	assetsOnce sync.Once

	faceReg16 font.Face
	faceReg18 font.Face
	faceReg20 font.Face
	faceReg22 font.Face
	faceReg24 font.Face
	faceReg28 font.Face
	faceReg32 font.Face

	faceBold18 font.Face
	faceBold20 font.Face
	faceBold22 font.Face
	faceBold24 font.Face
	faceBold28 font.Face
	faceBold32 font.Face
	faceBold36 font.Face
	faceBold44 font.Face
	faceBold48 font.Face
	faceBold56 font.Face

	rawLogoImg image.Image
)

func initUIAssets() {
	assetsOnce.Do(func() {
		// 1. Parse Inter TTF fonts
		fReg, err := opentype.Parse(fontRegularBytes)
		if err != nil {
			panic("failed to parse Inter-Regular: " + err.Error())
		}
		fBold, err := opentype.Parse(fontBoldBytes)
		if err != nil {
			panic("failed to parse Inter-Bold: " + err.Error())
		}

		makeFace := func(f *opentype.Font, size float64) font.Face {
			face, err := opentype.NewFace(f, &opentype.FaceOptions{
				Size:    size,
				DPI:     72,
				Hinting: font.HintingFull,
			})
			if err != nil {
				panic("failed to create face: " + err.Error())
			}
			return face
		}

		faceReg16 = makeFace(fReg, 16)
		faceReg18 = makeFace(fReg, 18)
		faceReg20 = makeFace(fReg, 20)
		faceReg22 = makeFace(fReg, 22)
		faceReg24 = makeFace(fReg, 24)
		faceReg28 = makeFace(fReg, 28)
		faceReg32 = makeFace(fReg, 32)

		faceBold18 = makeFace(fBold, 18)
		faceBold20 = makeFace(fBold, 20)
		faceBold22 = makeFace(fBold, 22)
		faceBold24 = makeFace(fBold, 24)
		faceBold28 = makeFace(fBold, 28)
		faceBold32 = makeFace(fBold, 32)
		faceBold36 = makeFace(fBold, 36)
		faceBold44 = makeFace(fBold, 44)
		faceBold48 = makeFace(fBold, 48)
		faceBold56 = makeFace(fBold, 56)

		// 2. Decode Logo
		if len(logoPNGBytes) > 0 {
			img, err := png.Decode(bytes.NewReader(logoPNGBytes))
			if err == nil {
				rawLogoImg = img
			}
		}

		// 3. Pre-render solid-color ad slide cards for zero-latency carousel transitions
		initAdSlides()
	})
}

// maskCardCorners clips the 4 corners of a rectangular region r with radius,
// anti-aliasing and blending any pixel outside the radius with the background color bg.
func maskCardCorners(dst *image.RGBA, r image.Rectangle, radius int, bg color.RGBA) {
	if radius <= 0 {
		return
	}
	w := r.Dx()
	h := r.Dy()
	if radius > w/2 {
		radius = w / 2
	}
	if radius > h/2 {
		radius = h / 2
	}

	rF := float64(radius)
	bgA := float64(bg.A) / 255.0

	maskCorner := func(cornerMinX, cornerMinY, cornerMaxX, cornerMaxY int, cx, cy float64) {
		for y := cornerMinY; y < cornerMaxY; y++ {
			fy := float64(y) + 0.5
			dy := fy - cy
			rowStart := y*dst.Stride + cornerMinX*4
			rowEnd := y*dst.Stride + cornerMaxX*4
			for p, x := rowStart, cornerMinX; p < rowEnd; p, x = p+4, x+1 {
				fx := float64(x) + 0.5
				dx := fx - cx
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > rF-0.5 {
					if dist >= rF+0.5 {
						dst.Pix[p+0] = bg.R
						dst.Pix[p+1] = bg.G
						dst.Pix[p+2] = bg.B
						dst.Pix[p+3] = bg.A
					} else {
						cardAlpha := (rF + 0.5) - dist
						invCardAlpha := 1.0 - cardAlpha
						dst.Pix[p+0] = uint8(float64(dst.Pix[p+0])*cardAlpha + float64(bg.R)*invCardAlpha*bgA)
						dst.Pix[p+1] = uint8(float64(dst.Pix[p+1])*cardAlpha + float64(bg.G)*invCardAlpha*bgA)
						dst.Pix[p+2] = uint8(float64(dst.Pix[p+2])*cardAlpha + float64(bg.B)*invCardAlpha*bgA)
						dst.Pix[p+3] = 255
					}
				}
			}
		}
	}

	cx0 := float64(r.Min.X + radius)
	cy0 := float64(r.Min.Y + radius)
	cx1 := float64(r.Max.X - radius)
	cy1 := float64(r.Max.Y - radius)

	maskCorner(r.Min.X, r.Min.Y, r.Min.X+radius, r.Min.Y+radius, cx0, cy0)
	maskCorner(r.Max.X-radius, r.Min.Y, r.Max.X, r.Min.Y+radius, cx1, cy0)
	maskCorner(r.Min.X, r.Max.Y-radius, r.Min.X+radius, r.Max.Y, cx0, cy1)
	maskCorner(r.Max.X-radius, r.Max.Y-radius, r.Max.X, r.Max.Y, cx1, cy1)
}

// drawText renders text starting at top-left position (x, y) using the given font face and color.
func drawText(dst *image.RGBA, face font.Face, text string, x, y int, col color.RGBA) {
	if face == nil || len(text) == 0 {
		return
	}
	ascent := face.Metrics().Ascent.Ceil()
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y + ascent)},
	}
	d.DrawString(text)
}

// measureTextWidth returns the horizontal pixel width of the text with the given face.
func measureTextWidth(face font.Face, text string) int {
	if face == nil || len(text) == 0 {
		return 0
	}
	d := &font.Drawer{Face: face}
	return d.MeasureString(text).Ceil()
}

// fillRoundedRect fills a rectangle with rounded corners using anti-aliased edge blending.
func fillRoundedRect(dst *image.RGBA, r image.Rectangle, radius int, col color.RGBA) {
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	w := r.Dx()
	h := r.Dy()
	if radius <= 0 {
		fillRect(dst, r, col)
		return
	}
	if radius > w/2 {
		radius = w / 2
	}
	if radius > h/2 {
		radius = h / 2
	}

	// 1. Fill solid center horizontal block (between top and bottom corner zones)
	if h > 2*radius {
		fillRect(dst, image.Rect(r.Min.X, r.Min.Y+radius, r.Max.X, r.Max.Y-radius), col)
	}

	// 2. Fill solid middle strips in top and bottom row zones
	if w > 2*radius {
		fillRect(dst, image.Rect(r.Min.X+radius, r.Min.Y, r.Max.X-radius, r.Min.Y+radius), col)
		fillRect(dst, image.Rect(r.Min.X+radius, r.Max.Y-radius, r.Max.X-radius, r.Max.Y), col)
	}

	// 3. Render 4 anti-aliased corners only
	drawCorner := func(cornerMinX, cornerMinY, cornerMaxX, cornerMaxY int, cx, cy float64) {
		rF := float64(radius)
		colA := float64(col.A) / 255.0
		for y := cornerMinY; y < cornerMaxY; y++ {
			fy := float64(y) + 0.5
			dy := fy - cy
			rowStart := y*dst.Stride + cornerMinX*4
			rowEnd := y*dst.Stride + cornerMaxX*4
			for p, x := rowStart, cornerMinX; p < rowEnd; p, x = p+4, x+1 {
				fx := float64(x) + 0.5
				dx := fx - cx
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > rF+0.5 {
					continue
				}
				alpha := 1.0
				if dist > rF-0.5 {
					alpha = (rF + 0.5) - dist
				}
				alpha *= colA
				if alpha >= 1.0 {
					dst.Pix[p+0] = col.R
					dst.Pix[p+1] = col.G
					dst.Pix[p+2] = col.B
					dst.Pix[p+3] = 255
				} else if alpha > 0 {
					invAlpha := 1.0 - alpha
					dst.Pix[p+0] = uint8(float64(col.R)*alpha + float64(dst.Pix[p+0])*invAlpha)
					dst.Pix[p+1] = uint8(float64(col.G)*alpha + float64(dst.Pix[p+1])*invAlpha)
					dst.Pix[p+2] = uint8(float64(col.B)*alpha + float64(dst.Pix[p+2])*invAlpha)
					dst.Pix[p+3] = 255
				}
			}
		}
	}

	cx0 := float64(r.Min.X + radius)
	cy0 := float64(r.Min.Y + radius)
	cx1 := float64(r.Max.X - radius)
	cy1 := float64(r.Max.Y - radius)

	// Top-left corner
	drawCorner(r.Min.X, r.Min.Y, r.Min.X+radius, r.Min.Y+radius, cx0, cy0)
	// Top-right corner
	drawCorner(r.Max.X-radius, r.Min.Y, r.Max.X, r.Min.Y+radius, cx1, cy0)
	// Bottom-left corner
	drawCorner(r.Min.X, r.Max.Y-radius, r.Min.X+radius, r.Max.Y, cx0, cy1)
	// Bottom-right corner
	drawCorner(r.Max.X-radius, r.Max.Y-radius, r.Max.X, r.Max.Y, cx1, cy1)
}

var bayer8 = [8][8]float64{
	{0.0 / 64.0, 32.0 / 64.0, 8.0 / 64.0, 40.0 / 64.0, 2.0 / 64.0, 34.0 / 64.0, 10.0 / 64.0, 42.0 / 64.0},
	{48.0 / 64.0, 16.0 / 64.0, 56.0 / 64.0, 24.0 / 64.0, 50.0 / 64.0, 18.0 / 64.0, 58.0 / 64.0, 26.0 / 64.0},
	{12.0 / 64.0, 44.0 / 64.0, 4.0 / 64.0, 36.0 / 64.0, 14.0 / 64.0, 46.0 / 64.0, 6.0 / 64.0, 38.0 / 64.0},
	{60.0 / 64.0, 28.0 / 64.0, 52.0 / 64.0, 20.0 / 64.0, 62.0 / 64.0, 30.0 / 64.0, 54.0 / 64.0, 22.0 / 64.0},
	{3.0 / 64.0, 35.0 / 64.0, 11.0 / 64.0, 43.0 / 64.0, 1.0 / 64.0, 33.0 / 64.0, 9.0 / 64.0, 41.0 / 64.0},
	{51.0 / 64.0, 19.0 / 64.0, 59.0 / 64.0, 27.0 / 64.0, 49.0 / 64.0, 17.0 / 64.0, 57.0 / 64.0, 25.0 / 64.0},
	{15.0 / 64.0, 47.0 / 64.0, 7.0 / 64.0, 39.0 / 64.0, 13.0 / 64.0, 45.0 / 64.0, 5.0 / 64.0, 37.0 / 64.0},
	{63.0 / 64.0, 31.0 / 64.0, 55.0 / 64.0, 23.0 / 64.0, 61.0 / 64.0, 29.0 / 64.0, 53.0 / 64.0, 21.0 / 64.0},
}

func clampByte(v float64) uint8 {
	if v <= 0.0 {
		return 0
	}
	if v >= 254.5 {
		return 255
	}
	return uint8(v + 0.5)
}

// interpolateGradient3Stop evaluates a continuous C1 Hermite spline across 3 color stops
// with spatial 8x8 Bayer dithering, eliminating both Mach band creasing at stop 1 and
// color quantization banding on 8-bit / low-color displays.
func interpolateGradient3Stop(t float64, c0, c1, c2 color.RGBA, x, y int) (uint8, uint8, uint8) {
	if t < 0.0 {
		t = 0.0
	} else if t > 1.0 {
		t = 1.0
	}

	r0, g0, b0 := float64(c0.R), float64(c0.G), float64(c0.B)
	r1, g1, b1 := float64(c1.R), float64(c1.G), float64(c1.B)
	r2, g2, b2 := float64(c2.R), float64(c2.G), float64(c2.B)

	mr := (r2 - r0) * 0.5
	mg := (g2 - g0) * 0.5
	mb := (b2 - b0) * 0.5

	var cr, cg, cb float64
	if t <= 0.5 {
		u := t * 2.0
		u2 := u * u
		u3 := u2 * u
		h00 := 2.0*u3 - 3.0*u2 + 1.0
		h10 := u3 - 2.0*u2 + u
		h01 := -2.0*u3 + 3.0*u2
		h11 := u3 - u2

		cr = h00*r0 + h10*(r1-r0) + h01*r1 + h11*mr
		cg = h00*g0 + h10*(g1-g0) + h01*g1 + h11*mg
		cb = h00*b0 + h10*(b1-b0) + h01*b1 + h11*mb
	} else {
		u := (t - 0.5) * 2.0
		u2 := u * u
		u3 := u2 * u
		h00 := 2.0*u3 - 3.0*u2 + 1.0
		h10 := u3 - 2.0*u2 + u
		h01 := -2.0*u3 + 3.0*u2
		h11 := u3 - u2

		cr = h00*r1 + h10*mr + h01*r2 + h11*(r2-r1)
		cg = h00*g1 + h10*mg + h01*g2 + h11*(g2-g1)
		cb = h00*b1 + h10*mb + h01*b2 + h11*(b2-b1)
	}

	d := (bayer8[y&7][x&7] - 0.5) * 0.75
	return clampByte(cr + d), clampByte(cg + d), clampByte(cb + d)
}

// fillLinearGradient135 fills a rectangle with a true 135-degree 3-stop linear gradient:
// c0 at 0% (top-left), c1 at 50% (center diagonal), c2 at 100% (bottom-right).
func fillLinearGradient135(dst *image.RGBA, r image.Rectangle, c0, c1, c2 color.RGBA) {
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	w := r.Dx()
	h := r.Dy()
	totalDist := float64(w + h)

	for y := r.Min.Y; y < r.Max.Y; y++ {
		yDist := float64(y - r.Min.Y)
		rowStart := y*dst.Stride + r.Min.X*4
		for x, p := r.Min.X, rowStart; x < r.Max.X; x, p = x+1, p+4 {
			t := (float64(x-r.Min.X) + yDist) / totalDist
			cr, cg, cb := interpolateGradient3Stop(t, c0, c1, c2, x, y)
			dst.Pix[p+0] = cr
			dst.Pix[p+1] = cg
			dst.Pix[p+2] = cb
			dst.Pix[p+3] = 255
		}
	}
}

// fillRoundedGradient fills a rectangle with rounded corners using a 135-degree 3-stop linear gradient.
// In CSS, 135deg points down-right (dx = dy in screen pixels).
func fillRoundedGradient(dst *image.RGBA, r image.Rectangle, radius int, c0, c1, c2 color.RGBA) {
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	w := r.Dx()
	h := r.Dy()
	if radius <= 0 {
		radius = 0
	}
	if radius > w/2 {
		radius = w / 2
	}
	if radius > h/2 {
		radius = h / 2
	}

	rF := float64(radius)
	cx0 := float64(r.Min.X + radius)
	cy0 := float64(r.Min.Y + radius)
	cx1 := float64(r.Max.X - radius)
	cy1 := float64(r.Max.Y - radius)

	// True 135deg CSS linear gradient:
	// Normalized position t along diagonal from top-left (x0, y0) to bottom-right (x1, y1):
	// t = ((x - x0) + (y - y0)) / (w + h)
	totalDist := float64(w + h)

	for y := r.Min.Y; y < r.Max.Y; y++ {
		yDist := float64(y - r.Min.Y)
		fy := float64(y) + 0.5
		isTop := y < r.Min.Y+radius
		isBottom := y >= r.Max.Y-radius

		rowStart := y*dst.Stride + r.Min.X*4
		for x, p := r.Min.X, rowStart; x < r.Max.X; x, p = x+1, p+4 {
			alpha := 1.0
			fx := float64(x) + 0.5

			if isTop && x < r.Min.X+radius {
				dx := fx - cx0
				dy := fy - cy0
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > rF+0.5 {
					continue
				}
				if dist > rF-0.5 {
					alpha = (rF + 0.5) - dist
				}
			} else if isTop && x >= r.Max.X-radius {
				dx := fx - cx1
				dy := fy - cy0
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > rF+0.5 {
					continue
				}
				if dist > rF-0.5 {
					alpha = (rF + 0.5) - dist
				}
			} else if isBottom && x < r.Min.X+radius {
				dx := fx - cx0
				dy := fy - cy1
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > rF+0.5 {
					continue
				}
				if dist > rF-0.5 {
					alpha = (rF + 0.5) - dist
				}
			} else if isBottom && x >= r.Max.X-radius {
				dx := fx - cx1
				dy := fy - cy1
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > rF+0.5 {
					continue
				}
				if dist > rF-0.5 {
					alpha = (rF + 0.5) - dist
				}
			}

			// True 135-degree continuous Hermite interpolation with 8x8 Bayer dithering
			t := (float64(x-r.Min.X) + yDist) / totalDist
			cr, cg, cb := interpolateGradient3Stop(t, c0, c1, c2, x, y)

			if alpha >= 1.0 {
				dst.Pix[p+0] = cr
				dst.Pix[p+1] = cg
				dst.Pix[p+2] = cb
				dst.Pix[p+3] = 255
			} else if alpha > 0 {
				invAlpha := 1.0 - alpha
				dst.Pix[p+0] = uint8(float64(cr)*alpha + float64(dst.Pix[p+0])*invAlpha + 0.5)
				dst.Pix[p+1] = uint8(float64(cg)*alpha + float64(dst.Pix[p+1])*invAlpha + 0.5)
				dst.Pix[p+2] = uint8(float64(cb)*alpha + float64(dst.Pix[p+2])*invAlpha + 0.5)
				dst.Pix[p+3] = 255
			}
		}
	}
}

// strokeRoundedRect draws a rounded rectangle border of the specified thickness.
func strokeRoundedRect(dst *image.RGBA, r image.Rectangle, radius, thickness int, col color.RGBA) {
	if thickness <= 0 {
		return
	}
	r = r.Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	w := r.Dx()
	h := r.Dy()
	if radius <= 0 {
		strokeRect(dst, r, thickness, col)
		return
	}
	if radius > w/2 {
		radius = w / 2
	}
	if radius > h/2 {
		radius = h / 2
	}

	// 1. Draw straight edges
	// Top edge
	if w > 2*radius {
		fillRect(dst, image.Rect(r.Min.X+radius, r.Min.Y, r.Max.X-radius, r.Min.Y+thickness), col)
	}
	// Bottom edge
	if w > 2*radius {
		fillRect(dst, image.Rect(r.Min.X+radius, r.Max.Y-thickness, r.Max.X-radius, r.Max.Y), col)
	}
	// Left edge
	if h > 2*radius {
		fillRect(dst, image.Rect(r.Min.X, r.Min.Y+radius, r.Min.X+thickness, r.Max.Y-radius), col)
	}
	// Right edge
	if h > 2*radius {
		fillRect(dst, image.Rect(r.Max.X-thickness, r.Min.Y+radius, r.Max.X, r.Max.Y-radius), col)
	}

	// 2. Render 4 corner curves
	rOuter := float64(radius)
	rInner := float64(radius - thickness)
	if rInner < 0 {
		rInner = 0
	}
	colA := float64(col.A) / 255.0

	drawCornerStroke := func(cornerMinX, cornerMinY, cornerMaxX, cornerMaxY int, cx, cy float64) {
		for y := cornerMinY; y < cornerMaxY; y++ {
			fy := float64(y) + 0.5
			dy := fy - cy
			rowStart := y*dst.Stride + cornerMinX*4
			rowEnd := y*dst.Stride + cornerMaxX*4
			for p, x := rowStart, cornerMinX; p < rowEnd; p, x = p+4, x+1 {
				fx := float64(x) + 0.5
				dx := fx - cx
				dist := math.Sqrt(dx*dx + dy*dy)
				if dist > rOuter+0.5 || (rInner > 0 && dist < rInner-0.5) {
					continue
				}
				alpha := 1.0
				if dist > rOuter-0.5 {
					alpha = (rOuter + 0.5) - dist
				} else if rInner > 0 && dist < rInner+0.5 {
					alpha = dist - (rInner - 0.5)
				}
				alpha *= colA
				if alpha >= 1.0 {
					dst.Pix[p+0] = col.R
					dst.Pix[p+1] = col.G
					dst.Pix[p+2] = col.B
					dst.Pix[p+3] = 255
				} else if alpha > 0 {
					invAlpha := 1.0 - alpha
					dst.Pix[p+0] = uint8(float64(col.R)*alpha + float64(dst.Pix[p+0])*invAlpha)
					dst.Pix[p+1] = uint8(float64(col.G)*alpha + float64(dst.Pix[p+1])*invAlpha)
					dst.Pix[p+2] = uint8(float64(col.B)*alpha + float64(dst.Pix[p+2])*invAlpha)
					dst.Pix[p+3] = 255
				}
			}
		}
	}

	cx0 := float64(r.Min.X + radius)
	cy0 := float64(r.Min.Y + radius)
	cx1 := float64(r.Max.X - radius)
	cy1 := float64(r.Max.Y - radius)

	// Top-left
	drawCornerStroke(r.Min.X, r.Min.Y, r.Min.X+radius, r.Min.Y+radius, cx0, cy0)
	// Top-right
	drawCornerStroke(r.Max.X-radius, r.Min.Y, r.Max.X, r.Min.Y+radius, cx1, cy0)
	// Bottom-left
	drawCornerStroke(r.Min.X, r.Max.Y-radius, r.Min.X+radius, r.Max.Y, cx0, cy1)
	// Bottom-right
	drawCornerStroke(r.Max.X-radius, r.Max.Y-radius, r.Max.X, r.Max.Y, cx1, cy1)
}

// drawPill draws a pill-shaped badge with background and stroke.
func drawPill(dst *image.RGBA, r image.Rectangle, bgCol, borderCol color.RGBA) {
	radius := r.Dy() / 2
	fillRoundedRect(dst, r, radius, bgCol)
	strokeRoundedRect(dst, r, radius, 1, borderCol)
}

// drawCircle fills an anti-aliased circle.
func drawCircle(dst *image.RGBA, cx, cy, radius int, col color.RGBA) {
	r := image.Rect(cx-radius, cy-radius, cx+radius, cy+radius)
	fillRoundedRect(dst, r, radius, col)
}

// drawLogo scales and draws the embedded XeroxGo logo into dst at (x, y) with the specified target height.
func drawLogo(dst *image.RGBA, x, y, targetHeight int) {
	if rawLogoImg == nil {
		return
	}
	b := rawLogoImg.Bounds()
	if b.Empty() {
		return
	}
	targetWidth := (targetHeight * b.Dx()) / b.Dy()
	dstRect := image.Rect(x, y, x+targetWidth, y+targetHeight)
	draw.BiLinear.Scale(dst, dstRect, rawLogoImg, b, draw.Over, nil)
}

// drawCloseButton renders a circular close button with a crisp 2px "×" icon.
func drawCloseButton(dst *image.RGBA, cx, cy, radius int) {
	drawCircle(dst, cx, cy, radius, color.RGBA{R: 255, G: 255, B: 255, A: 50})
	arm := radius * 4 / 10
	if arm < 3 {
		arm = 3
	}
	col := color.RGBA{R: 255, G: 255, B: 255, A: 230}
	for d := -arm; d <= arm; d++ {
		setPixelClamped(dst, cx+d, cy+d, col)
		setPixelClamped(dst, cx+d+1, cy+d, col)
		setPixelClamped(dst, cx+d, cy-d, col)
		setPixelClamped(dst, cx+d+1, cy-d, col)
	}
}

func setPixelClamped(dst *image.RGBA, x, y int, col color.RGBA) {
	if x >= dst.Bounds().Min.X && x < dst.Bounds().Max.X && y >= dst.Bounds().Min.Y && y < dst.Bounds().Max.Y {
		offset := y*dst.Stride + x*4
		if col.A == 255 {
			dst.Pix[offset+0] = col.R
			dst.Pix[offset+1] = col.G
			dst.Pix[offset+2] = col.B
			dst.Pix[offset+3] = 255
		} else if col.A > 0 {
			a := float64(col.A) / 255.0
			inv := 1.0 - a
			dst.Pix[offset+0] = uint8(float64(col.R)*a + float64(dst.Pix[offset+0])*inv)
			dst.Pix[offset+1] = uint8(float64(col.G)*a + float64(dst.Pix[offset+1])*inv)
			dst.Pix[offset+2] = uint8(float64(col.B)*a + float64(dst.Pix[offset+2])*inv)
			dst.Pix[offset+3] = 255
		}
	}
}
