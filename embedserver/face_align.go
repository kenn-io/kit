package embedserver

import (
	"errors"
	"image"
	"math"

	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
)

type point struct{ x, y float64 }

var canonicalPoints = [5]point{{38.2946, 51.6963}, {73.5318, 51.5014}, {56.0252, 71.7366}, {41.5493, 92.3655}, {70.7299, 92.2041}}

// similarityTransform fits a five-point, orientation-preserving transform.
func similarityTransform(source [5]point) (f64.Aff3, error) {
	var from, to point
	for i, p := range source {
		from.x += p.x / 5
		from.y += p.y / 5
		to.x += canonicalPoints[i].x / 5
		to.y += canonicalPoints[i].y / 5
	}
	var variance, dot, cross float64
	for i, p := range source {
		fx, fy := p.x-from.x, p.y-from.y
		tx, ty := canonicalPoints[i].x-to.x, canonicalPoints[i].y-to.y
		variance += fx*fx + fy*fy
		dot += fx*tx + fy*ty
		cross += fx*ty - fy*tx
	}
	if variance <= 1e-6 {
		return f64.Aff3{}, errors.New("degenerate face landmarks")
	}
	a, b := dot/variance, cross/variance
	if math.Hypot(a, b) <= 1e-9 || math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return f64.Aff3{}, errors.New("invalid face transform")
	}
	return f64.Aff3{a, -b, to.x - a*from.x + b*from.y, b, a, to.y - b*from.x - a*from.y}, nil
}

func alignedFace(src image.Image, points [5]point) (*image.RGBA, error) {
	transform, err := similarityTransform(points)
	if err != nil {
		return nil, err
	}
	out := image.NewRGBA(image.Rect(0, 0, 112, 112))
	draw.BiLinear.Transform(out, transform, src, src.Bounds(), draw.Src, nil)
	return out, nil
}

// bgrPlanes preserves the model's BGR ordering and scale.
func bgrPlanes(src image.Image, normalize bool) []float32 {
	bounds := src.Bounds()
	size := bounds.Dx() * bounds.Dy()
	out := make([]float32, size*3)
	for y := range bounds.Dy() {
		for x := range bounds.Dx() {
			r, g, b, _ := src.At(x+bounds.Min.X, y+bounds.Min.Y).RGBA()
			for c, v := range []uint32{b, g, r} {
				value := float32(v >> 8)
				if normalize {
					value = (value - 127.5) / 128
				}
				out[c*size+y*bounds.Dx()+x] = value
			}
		}
	}
	return out
}
