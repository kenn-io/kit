package embedassets

import (
	"fmt"
	"path/filepath"
)

const (
	TextModel        = "nomic-embed-text"
	FaceModel        = "auraface-v1"
	FaceRevision     = "af6d057c9b0ec4071d4c49c80e3539258798b609"
	DetectorRevision = "f12e12798e8314f7c074a6656816c048dcc95b7a"
	FaceRecipe       = "yunet-0.6-320-arcface5-bgr-127.5-128-v1"
)

// SelectedModel returns the installed directory and exact downloads for a model.
func SelectedModel(dir, model string) (string, []Asset, error) {
	switch model {
	case "", TextModel:
		return ModelDir(dir), ModelAssets(), nil
	case FaceModel:
		return filepath.Join(dir, FaceModel, FaceRevision), []Asset{
			{"glintr100.onnx", "https://huggingface.co/fal/AuraFace-v1/resolve/" + FaceRevision + "/glintr100.onnx", "a7933ea5330113b01c9b60351d8f4c33003f145d8470ac5f0e52ee2effe25c60", 260694151},
			{"yunet.onnx", "https://github.com/opencv/opencv_zoo/raw/" + DetectorRevision + "/models/face_detection_yunet/face_detection_yunet_2023mar.onnx", "8f2383e4dd3cfbb4553ea8718107fc0423210dc964f9f4280604804ed2552fa4", 232589},
		}, nil
	default:
		return "", nil, fmt.Errorf("unknown embedding model %q", model)
	}
}

// InstalledModel verifies locally installed bytes without downloading anything.
func InstalledModel(dir, model string) (string, error) {
	path, assets, err := SelectedModel(dir, model)
	if err != nil {
		return "", err
	}
	missing, err := MissingModel(path, assets)
	if err != nil {
		return "", err
	}
	if len(missing) != 0 {
		return "", fmt.Errorf("install %s: missing or corrupt %s", model, missing[0].Name)
	}
	return path, nil
}
