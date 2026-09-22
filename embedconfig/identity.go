package embedconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// VectorIdentity identifies a comparable vector space.
// Empty input type is treated as none. The endpoint is included only when
// PinEndpoint is set. Batch, transport, retrieval, serving, and the wire
// encoding format are not inputs. Float and base64 are the same numbers.
func VectorIdentity(m Model, r Roles, d Deployment) (string, error) {
	fields, err := vectorFields(m, r, d)
	if err != nil {
		return "", err
	}
	return hashFields(fields), nil
}

// InputIdentity identifies indexed inputs. It includes the vector-space fields,
// because a model or role change also requires those inputs to be rebuilt, plus
// the recipe, tokenizer, content selection, and token window. Operational
// limits are not included.
func InputIdentity(m Model, r Roles, d Deployment, in InputLimits) (string, error) {
	fields, err := vectorFields(m, r, d)
	if err != nil {
		return "", err
	}
	in = in.normalize()
	if err := in.Validate(); err != nil {
		return "", err
	}
	add(fields, "recipe", in.Recipe)
	add(fields, "tokenizer", in.Tokenizer)
	add(fields, "tokenizer_revision", in.TokenizerRevision)
	add(fields, "content", in.ContentID)
	if in.MaxTokens > 0 {
		fields["max_tokens"] = strconv.Itoa(in.MaxTokens)
		fields["overlap_tokens"] = strconv.Itoa(in.OverlapTokens)
		fields["max_spans"] = strconv.Itoa(in.MaxSpans)
		fields["truncation"] = string(in.Truncation)
	}
	return hashFields(fields), nil
}

func vectorFields(m Model, r Roles, d Deployment) (map[string]string, error) {
	m = m.normalize()
	r = r.normalize()
	d = d.normalize()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	endpoint := ""
	if d.PinEndpoint || d.BaseURL != "" {
		if err := d.Validate(); err != nil {
			return nil, err
		}
	}
	if d.PinEndpoint {
		canonical, err := d.Canonical()
		if err != nil {
			return nil, err
		}
		endpoint = canonical
	}
	fields := map[string]string{
		"model":         m.Name,
		"dimensions":    strconv.Itoa(m.Dimensions),
		"metric":        string(m.Metric),
		"normalization": string(m.Normalization),
		"input_type":    string(r.normalizedType()),
	}
	add(fields, "revision", m.Revision)
	add(fields, "pooling", m.Pooling)
	if m.RequestDimensions {
		fields["request_dimensions"] = "1"
	}
	add(fields, "document_formatter", r.DocumentFormatter)
	add(fields, "query_formatter", r.QueryFormatter)
	add(fields, "document_prefix", r.DocumentPrefix)
	add(fields, "document_suffix", r.DocumentSuffix)
	add(fields, "query_prefix", r.QueryPrefix)
	add(fields, "query_suffix", r.QuerySuffix)
	add(fields, "endpoint", endpoint)
	return fields, nil
}

func add(fields map[string]string, key, value string) {
	if value == "" {
		return
	}
	fields[key] = value
}

func hashFields(fields map[string]string) string {
	keys := slices.Sorted(maps.Keys(fields))
	var b strings.Builder
	b.WriteString("v1\n")
	for _, key := range keys {
		b.WriteString(escape(key))
		b.WriteByte('=')
		b.WriteString(escape(fields[key]))
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func escape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, "\n", `\n`)
}
