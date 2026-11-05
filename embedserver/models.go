package embedserver

import (
	"context"
	"errors"
	"image"
	"path/filepath"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/internal/embedassets"
)

// models is used only while the shared inference gate is held.
type models struct {
	dir, library string
	text         engine
	faces        *faceModel
}

func loadModels(_ context.Context, dir, library string) (engine, error) {
	path, err := filepath.Abs(library)
	if err != nil {
		return nil, err
	}
	return &models{dir: dir, library: path}, nil
}

func (m *models) embed(ctx context.Context, text string) ([]float32, int, error) {
	if m.text == nil {
		model, err := loadModel(ctx, m.dir, m.library)
		if err != nil {
			return nil, 0, err
		}
		m.text = model
	}
	return m.text.embed(ctx, text)
}

func (m *models) face(ctx context.Context) (*faceModel, error) {
	if m.faces == nil {
		dir, err := embedassets.InstalledModel(m.dir, embedassets.FaceModel)
		if err != nil {
			return nil, err
		}
		detector, err := openORT(m.library, filepath.Join(dir, "yunet.onnx"))
		if err != nil {
			return nil, err
		}
		recognizer, err := openORT(m.library, filepath.Join(dir, "glintr100.onnx"))
		if err != nil {
			return nil, errors.Join(err, detector.Close())
		}
		m.faces = &faceModel{detector: detector, recognizer: recognizer}
	}
	return m.faces, ctx.Err()
}

func (m *models) Close() error {
	var err error
	if m.text != nil {
		err = m.text.Close()
	}
	if m.faces != nil {
		err = errors.Join(err, m.faces.detector.Close(), m.faces.recognizer.Close())
	}
	return err
}

func (m *models) embedFace(ctx context.Context, preview image.Image, box embedclient.FaceBox) ([]float32, error) {
	face, err := m.face(ctx)
	if err != nil {
		return nil, err
	}
	return face.embed(ctx, preview, box)
}
