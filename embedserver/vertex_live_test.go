package embedserver

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

func TestVertexLiveMedia(t *testing.T) {
	project := os.Getenv("KIT_EMBED_LIVE_VERTEX_PROJECT")
	if project == "" {
		t.Skip("set KIT_EMBED_LIVE_VERTEX_PROJECT to run")
	}
	location := os.Getenv("KIT_EMBED_LIVE_VERTEX_LOCATION")
	if location == "" {
		location = "global"
	}
	videoPath := os.Getenv("KIT_EMBED_LIVE_VERTEX_VIDEO")
	require.NotEmpty(t, videoPath)
	video, err := os.ReadFile(videoPath)
	require.NoError(t, err)
	pngBytes := func(c color.RGBA) []byte {
		img := image.NewRGBA(image.Rect(0, 0, 64, 64))
		for y := 0; y < 64; y++ {
			for x := 0; x < 64; x++ {
				img.SetRGBA(x, y, c)
			}
		}
		var b bytes.Buffer
		require.NoError(t, png.Encode(&b, img))
		return b.Bytes()
	}
	red := pngBytes(color.RGBA{R: 255, A: 255})
	blue := pngBytes(color.RGBA{B: 255, A: 255})
	pdf := livePDF()
	base, err := embedconfig.VertexBaseURL(project, location)
	require.NoError(t, err)
	google, err := embedclient.GoogleHTTPClient()
	require.NoError(t, err)
	model := embedconfig.Model{Name: "gemini-embedding-2", Dimensions: 768, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2}
	roles := embedconfig.Roles{QueryPrefix: "task: search result | query: "}
	vertex, err := embedclient.New(embedclient.Options{Model: model, Roles: roles, Deployment: embedconfig.Deployment{API: embedconfig.APIVertex, BaseURL: base}, HTTP: google})
	require.NoError(t, err)
	server := httptest.NewServer(Handler(vertex, []string{model.Name}))
	t.Cleanup(server.Close)
	kit, err := embedclient.New(embedclient.Options{Model: model, Roles: roles, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: server.URL}})
	require.NoError(t, err)
	inputs := []embedclient.Input{{Role: embedconfig.RoleQuery, Parts: []embedclient.Part{{Text: "a solid red square"}}}, {Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "image/png", Data: red}}}, {Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "image/png", Data: blue}}}, {Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "application/pdf", Data: pdf}}}, {Role: embedconfig.RoleDocument, Parts: []embedclient.Part{{MediaType: "video/mp4", Data: video}}}}
	for _, client := range []*embedclient.Client{vertex, kit} {
		vectors, err := client.EmbedInputs(t.Context(), inputs)
		require.NoError(t, err)
		require.Len(t, vectors, len(inputs))
		var redScore, blueScore float64
		for i, x := range vectors[0] {
			redScore += float64(x * vectors[1][i])
			blueScore += float64(x * vectors[2][i])
		}
		assert.Greater(t, redScore, blueScore)
	}
}

func livePDF() []byte {
	stream := "BT /F1 12 Tf 20 100 Td (A red square) Tj ET\n"
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream)}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, off := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return b.Bytes()
}
