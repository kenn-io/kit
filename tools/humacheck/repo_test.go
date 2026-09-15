package humacheck

import (
	"slices"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mapFS(files map[string]string) (fstest.MapFS, []string) {
	fsys := fstest.MapFS{}
	var tracked []string
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
		tracked = append(tracked, name)
	}
	slices.Sort(tracked)
	return fsys, tracked
}

const yamlSpec = "openapi: 3.1.0\ninfo:\n  title: t\n"
const jsonSpec = `{"openapi": "3.1.0", "info": {"title": "t"}}`

func TestCheckSpecs(t *testing.T) {
	t.Parallel()

	t.Run("json document flagged and yaml counted", func(t *testing.T) {
		t.Parallel()
		fsys, tracked := mapFS(map[string]string{
			"go.mod":                           "module m\n",
			"api/openapi.yaml":                 yamlSpec,
			"internal/client/openapi-3.0.json": jsonSpec,
			"config/settings.json":             `{"openapi": false}`,
			"docs/other.yml":                   "openapi: guide\n",
			"node_modules/dep/openapi.json":    jsonSpec,
			"testdata/fixture.json":            jsonSpec,
		})
		diags := checkSpecs(fsys, tracked, true, "go.mod")
		require.Len(t, diags, 1)
		assert.Equal(t, Diagnostic{Path: "internal/client/openapi-3.0.json", Line: 1, Column: 1, Rule: RuleSpec, Message: specJSONMessage}, diags[0])
	})

	t.Run("missing document reported at go.mod only with an API", func(t *testing.T) {
		t.Parallel()
		assert := assert.New(t)
		fsys, tracked := mapFS(map[string]string{"go.mod": "module m\n"})
		diags := checkSpecs(fsys, tracked, true, "go.mod")
		require.Len(t, diags, 1)
		assert.Equal("go.mod", diags[0].Path)
		assert.Equal(specMissingMessage, diags[0].Message)
		assert.Empty(checkSpecs(fsys, tracked, false, "go.mod"))
	})

	t.Run("nested module path anchors the finding", func(t *testing.T) {
		t.Parallel()
		fsys, tracked := mapFS(map[string]string{"apps/api/go.mod": "module m\n"})
		diags := checkSpecs(fsys, tracked, true, "apps/api/go.mod")
		require.Len(t, diags, 1)
		assert.Equal(t, "apps/api/go.mod", diags[0].Path)
	})
}

func TestCheckGenerators(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		files    map[string]string
		hasAPI   bool
		wantMsgs []string
	}{
		{
			name:   "standard go generator in go.mod satisfies",
			files:  map[string]string{"go.mod": "module m\n\nrequire github.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n"},
			hasAPI: true,
		},
		{
			name:   "orval in package.json satisfies",
			files:  map[string]string{"go.mod": "module m\n", "web/package.json": `{"devDependencies": {"orval": "8.26.0"}}`},
			hasAPI: true,
		},
		{
			name:   "generate directive satisfies",
			files:  map[string]string{"go.mod": "module m\n", "pkg/client/generate.go": "//go:generate go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen -config c.yaml spec.yaml\n"},
			hasAPI: true,
		},
		{
			name:     "no generator with API",
			files:    map[string]string{"go.mod": "module m\n"},
			hasAPI:   true,
			wantMsgs: []string{generatorMissing},
		},
		{
			name:   "no generator without API",
			files:  map[string]string{"go.mod": "module m\n"},
			hasAPI: false,
		},
		{
			name: "nonstandard generators reported and do not satisfy",
			files: map[string]string{
				"go.mod":           "module m\n\nrequire github.com/oapi-codegen/oapi-codegen/v2 v2.8.0\n",
				"web/package.json": `{"devDependencies": {"openapi-typescript": "7.13.0", "openapi-fetch": "0.17.0"}}`,
			},
			hasAPI:   true,
			wantMsgs: []string{nonstandardGenerators[0].message, nonstandardGenerators[2].message, generatorMissing},
		},
		{
			name:   "indirect requirement is not a choice",
			files:  map[string]string{"go.mod": "module m\n\nrequire (\n\tgithub.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n\tgithub.com/oapi-codegen/oapi-codegen/v2 v2.8.0 // indirect\n)\n"},
			hasAPI: true,
		},
		{
			name: "banned generators flagged",
			files: map[string]string{
				"go.mod":           "module m\n\nrequire github.com/deepmap/oapi-codegen v1.16.0\n",
				"web/package.json": `{"devDependencies": {"@openapitools/openapi-generator-cli": "2.0.0", "orval": "8.26.0"}}`,
			},
			hasAPI:   true,
			wantMsgs: []string{bannedGenerators[0].message, bannedGenerators[2].message},
		},
		{
			name:   "node_modules ignored",
			files:  map[string]string{"go.mod": "module m\n\nrequire github.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n", "web/node_modules/x/package.json": `{"name": "swagger-typescript-api"}`},
			hasAPI: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys, tracked := mapFS(tt.files)
			diags := checkGenerators(fsys, tracked, tt.hasAPI, "go.mod")
			var msgs []string
			for _, d := range diags {
				assert.Equal(t, RuleGenerator, d.Rule)
				msgs = append(msgs, d.Message)
			}
			assert.ElementsMatch(t, tt.wantMsgs, msgs)
		})
	}
}

func TestCheckFrontend(t *testing.T) {
	t.Parallel()
	routes := testRoutes("/ping", "/accounts/{id}", "/jobs")
	fsys, tracked := mapFS(map[string]string{
		"frontend/src/lib/api/client.ts": "" +
			"export async function ping(base: string) {\n" +
			"  const res = await fetch(`${base}/api/v1/ping`);\n" +
			"  return res.json();\n" +
			"}\n" +
			"export const account = (id: string) =>\n" +
			"  fetch(\n" +
			"    '/api/v1/accounts/' + id,\n" +
			"  );\n" +
			"// fetch('/api/v1/jobs') in a comment is ignored\n" +
			"export const link = '/api/v1/jobs';\n",
		"frontend/src/lib/api/generated/client.ts": "fetch('/api/v1/ping');\n",
		"frontend/src/lib/api/client.test.ts":      "fetch('/api/v1/ping');\n",
		"frontend/node_modules/x/index.js":         "fetch('/api/v1/ping');\n",
		"frontend/src/App.svelte":                  "<script>\n  const r = fetch(\"/api/v1/jobs\");\n</script>\n",
	})
	diags := checkFrontend(fsys, tracked, routes)
	var got []string
	for _, d := range diags {
		assert.Equal(t, RuleFrontend, d.Rule)
		got = append(got, d.String())
	}
	assert.Equal(t, []string{
		"frontend/src/App.svelte:2:19: " + formatClientMessage("/api/v1/jobs") + " (frontend)",
		"frontend/src/lib/api/client.ts:2:27: " + formatClientMessage("/api/v1/ping") + " (frontend)",
		"frontend/src/lib/api/client.ts:7:5: " + formatClientMessage("/api/v1/accounts/{param}") + " (frontend)",
	}, got)
}

func TestFrontendSourceFile(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	assert.True(frontendSourceFile("web/src/lib/api.ts"))
	assert.True(frontendSourceFile("web/src/App.svelte"))
	assert.False(frontendSourceFile("web/src/lib/api.d.ts"))
	assert.False(frontendSourceFile("web/src/lib/api.test.ts"))
	assert.False(frontendSourceFile("web/src/lib/generated/api.ts"))
	assert.False(frontendSourceFile("web/e2e/smoke.ts"))
	assert.False(frontendSourceFile("web/src/lib/api.go"))
}
