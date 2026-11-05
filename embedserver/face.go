package embedserver

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"

	"go.kenn.io/kit/embedclient"
	"golang.org/x/image/draw"
)

type faceModel struct{ detector, recognizer *ort }

func (m *faceModel) embed(ctx context.Context, preview image.Image, box embedclient.FaceBox) ([]float32, error) {
	bounds := preview.Bounds()
	crop := image.Rect(int(math.Floor(box.X*float64(bounds.Dx()))), int(math.Floor(box.Y*float64(bounds.Dy()))), int(math.Ceil((box.X+box.Width)*float64(bounds.Dx()))), int(math.Ceil((box.Y+box.Height)*float64(bounds.Dy())))).Intersect(bounds)
	if crop.Empty() {
		return nil, nil
	}
	scale := 320 / float64(max(crop.Dx(), crop.Dy()))
	width, height := max(1, int(math.Round(float64(crop.Dx())*scale))), max(1, int(math.Round(float64(crop.Dy())*scale)))
	resized := image.NewRGBA(image.Rect(0, 0, 640, 640))
	draw.BiLinear.Scale(resized, image.Rect(0, 0, width, height), preview, crop, draw.Src, nil)
	names := make([]string, 0, 12)
	lengths := make([]int, 0, 12)
	for _, kind := range []struct {
		name     string
		channels int
	}{{"cls", 1}, {"obj", 1}, {"bbox", 4}, {"kps", 10}} {
		for _, stride := range []int{8, 16, 32} {
			names = append(names, fmt.Sprintf("%s_%d", kind.name, stride))
			lengths = append(lengths, (640/stride)*(640/stride)*kind.channels)
		}
	}
	outputs, err := m.detector.inferFloat(ctx, "input", []int64{1, 3, 640, 640}, bgrPlanes(resized, false), names, lengths)
	if err != nil {
		return nil, err
	}
	points, found := selectLandmarks(outputs, width, height, crop)
	if !found {
		return nil, nil
	}
	aligned, err := alignedFace(preview, points)
	if err != nil {
		return nil, err
	}
	vectors, err := m.recognizer.inferFloat(ctx, "data", []int64{1, 3, 112, 112}, bgrPlanes(aligned, true), []string{"1333"}, []int{512})
	if err != nil {
		return nil, err
	}
	vector := vectors[0]
	var squared float64
	for _, v := range vector {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, errors.New("face vector is not finite")
		}
		squared += float64(v) * float64(v)
	}
	if squared == 0 {
		return nil, errors.New("face vector has zero norm")
	}
	norm := math.Sqrt(squared)
	for i, v := range vector {
		vector[i] = float32(float64(v) / norm)
	}
	return vector, nil
}

// selectLandmarks follows OpenCV's YuNet head decoder. Greatest IoU selects
// the requested face, even when another face in the crop scores higher.
func selectLandmarks(outputs [][]float32, width, height int, crop image.Rectangle) ([5]point, bool) {
	var best [5]point
	bestOverlap, bestScore := 0.0, 0.0
	for level, stride := range []int{8, 16, 32} {
		columns := 640 / stride
		for index, cls := range outputs[level] {
			score := math.Sqrt(min(1, max(0, float64(cls))) * min(1, max(0, float64(outputs[level+3][index]))))
			if score < 0.6 || math.IsNaN(score) {
				continue
			}
			row, col := index/columns, index%columns
			bbox := outputs[level+6][index*4 : index*4+4]
			cx, cy := (float64(col)+float64(bbox[0]))*float64(stride), (float64(row)+float64(bbox[1]))*float64(stride)
			w, h := math.Exp(float64(bbox[2]))*float64(stride), math.Exp(float64(bbox[3]))*float64(stride)
			intersection := max(0, min(float64(width), cx+w/2)-max(0, cx-w/2)) * max(0, min(float64(height), cy+h/2)-max(0, cy-h/2))
			overlap := intersection / (w*h + float64(width*height) - intersection)
			if overlap <= 0 || math.IsNaN(overlap) || overlap < bestOverlap || (overlap == bestOverlap && score <= bestScore) {
				continue
			}
			bestOverlap, bestScore = overlap, score
			kps := outputs[level+9][index*10 : index*10+10]
			for i := range best {
				best[i] = point{float64(crop.Min.X) + (float64(kps[i*2])+float64(col))*float64(stride)*float64(crop.Dx())/float64(width), float64(crop.Min.Y) + (float64(kps[i*2+1])+float64(row))*float64(stride)*float64(crop.Dy())/float64(height)}
			}
		}
	}
	return best, bestOverlap > 0
}
