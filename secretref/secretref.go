// Package secretref reads a secret named by one typed configuration value.
//
// In TOML a Ref is either the secret itself or a table naming its source:
//
//	api_key = "sk-inline-value"              # the secret itself
//	api_key = { env = "OPENAI_API_KEY" }     # environment variable
//	api_key = { file = "~/.config/app.key" } # private file; ~/ is home
//
// A string is always the literal secret. Every other source is a table field,
// so a new kind of source, such as a secret manager or a credential helper,
// is a new field and never changes what an existing value means. The table
// form { value = "..." } also holds the literal secret; encoders that write
// struct fields instead of calling MarshalTOML produce it.
//
// Ref decodes with any TOML library that supports encoding.TextUnmarshaler
// for strings and tagged struct fields for tables, and with encoding/json.
package secretref

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/safefileio"
)

// Ref holds a secret or names where it comes from. The zero value is unset.
// Set at most one source; Validate reports more than one.
type Ref struct { //nolint:recvcheck // decoders need pointer receivers; values marshal and resolve as copies
	// Value is the secret itself.
	Value string `toml:"value,omitempty" json:"value,omitempty"`
	// Env names an environment variable that holds the secret.
	Env string `toml:"env,omitempty" json:"env,omitempty"`
	// File is a private file that holds the secret. A leading ~/ means the
	// user's home directory.
	File string `toml:"file,omitempty" json:"file,omitempty"`
}

// Literal returns a Ref that holds the secret itself.
func Literal(secret string) Ref { return Ref{Value: secret} }

// IsZero reports whether the reference is unset.
func (r Ref) IsZero() bool { return r == Ref{} }

// Validate checks source exclusivity and selector shape without reading a source.
func (r Ref) Validate() error {
	var sources []string
	if r.Value != "" {
		sources = append(sources, "value")
	}
	if r.Env != "" {
		sources = append(sources, "env")
	}
	if r.File != "" {
		sources = append(sources, "file")
	}
	if len(sources) > 1 {
		return fmt.Errorf("secretref: set one source, not %s", strings.Join(sources, " and "))
	}
	if r.File != "" && (!filepath.IsAbs(r.File) && !strings.HasPrefix(r.File, "~/") || strings.ContainsRune(r.File, 0)) {
		return &SourceError{Source: "file:" + r.File, Reason: "file must use an absolute or ~/ path without NUL"}
	}
	if r.Env != "" && (strings.TrimSpace(r.Env) == "" || strings.ContainsAny(r.Env, "=\x00")) {
		return &SourceError{Source: "environment", Reason: "invalid environment variable name"}
	}
	return nil
}

// UnmarshalText decodes the string form: the secret itself.
func (r *Ref) UnmarshalText(text []byte) error {
	*r = Literal(string(text))
	return nil
}

// UnmarshalTOML decodes a string or a table for TOML decoders that call it
// before trying encoding.TextUnmarshaler, which would reject a table.
func (r *Ref) UnmarshalTOML(data any) error {
	switch v := data.(type) {
	case string:
		*r = Literal(v)
		return nil
	case map[string]any:
		var ref Ref
		for key, raw := range v {
			value, ok := raw.(string)
			if !ok {
				return fmt.Errorf("secretref: %s must be a string", key)
			}
			switch key {
			case "value":
				ref.Value = value
			case "env":
				ref.Env = value
			case "file":
				ref.File = value
			default:
				return fmt.Errorf("secretref: unknown secret source %q", key)
			}
		}
		*r = ref
		return r.Validate()
	default:
		return fmt.Errorf("secretref: a secret is a string or a table, got %T", data)
	}
}

// UnmarshalJSON decodes a JSON string or object.
func (r *Ref) UnmarshalJSON(data []byte) error {
	var literal string
	if err := json.Unmarshal(data, &literal); err == nil {
		*r = Literal(literal)
		return nil
	}
	invalid := errors.New("secretref: a secret is a string or an object with known string fields")
	var fields map[string]*string
	if err := json.Unmarshal(data, &fields); err != nil {
		return invalid
	}
	var decoded Ref
	for key, value := range fields {
		if value == nil {
			return invalid
		}
		switch key {
		case "value":
			decoded.Value = *value
		case "env":
			decoded.Env = *value
		case "file":
			decoded.File = *value
		default:
			return invalid
		}
	}
	*r = decoded
	return r.Validate()
}

// MarshalTOML encodes the reference in the form the decoders read.
func (r Ref) MarshalTOML() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	switch {
	case r.Env != "":
		return []byte("{ env = " + quote(r.Env) + " }"), nil
	case r.File != "":
		return []byte("{ file = " + quote(r.File) + " }"), nil
	default:
		return []byte(quote(r.Value)), nil
	}
}

// Resolve reads the secret. An unset Ref resolves to an empty Secret.
// A configured environment or file source that is missing, empty, or
// unreadable returns an error. A reference naming more than one source
// also returns an error.
//
// Files follow safefileio.OpenSecretFile's trusted-path policy, are bounded to
// 64 KiB, and lose trailing CR/LF bytes. Other bytes are preserved. Blank and
// NUL-containing secrets are errors; exact-empty literals remain unset.
func (r Ref) Resolve() (Secret, error) {
	return r.ResolveContext(context.Background())
}

// ResolveContext resolves a source, checking cancellation between I/O steps.
// Cancellation cannot interrupt every operating-system filesystem call.
func (r Ref) ResolveContext(ctx context.Context) (Secret, error) {
	if err := ctx.Err(); err != nil {
		return Secret{}, err
	}
	if err := r.Validate(); err != nil {
		return Secret{}, err
	}
	switch {
	case r.IsZero():
		return Secret{}, nil
	case r.Env != "":
		name := strings.TrimSpace(r.Env)
		return checkedSecret(os.Getenv(name), "env:"+name)
	case r.File != "":
		return resolveFile(ctx, r.File)
	default:
		return checkedSecret(r.Value, "inline")
	}
}

func checkedSecret(value, source string) (Secret, error) {
	if strings.TrimSpace(value) == "" {
		return Secret{}, &SourceError{Source: source, Reason: "secret is unset or empty"}
	}
	if strings.ContainsRune(value, 0) {
		return Secret{}, &SourceError{Source: source, Reason: "secret contains a NUL byte"}
	}
	return Secret{Value: value, Source: source}, nil
}

func resolveFile(ctx context.Context, configured string) (Secret, error) {
	unavailable := func(reason string) (Secret, error) {
		return Secret{}, &SourceError{Source: "file:" + configured, Reason: reason}
	}
	path := configured
	if strings.HasPrefix(configured, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return unavailable("file home directory is unavailable")
		}
		// Joining would clean .. before the trusted walker can inspect its
		// preceding components, and changes the meaning after a symlink.
		path = home + string(os.PathSeparator) + strings.TrimPrefix(configured, "~/")
	}
	file, err := safefileio.OpenSecretFile(ctx, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return unavailable("file is missing")
		}
		if ctx.Err() != nil {
			return Secret{}, ctx.Err()
		}
		return unavailable("file must be a regular file with trusted ownership, private permissions and parents")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return unavailable("file is unreadable")
	}
	if info.Size() > 64<<10 {
		return unavailable("file exceeds 64 KiB")
	}
	data, err := readSecret(ctx, file)
	if err != nil {
		if ctx.Err() != nil {
			return Secret{}, ctx.Err()
		}
		return unavailable(err.Error())
	}
	value := strings.TrimRight(string(data), "\r\n")
	if strings.TrimSpace(value) == "" {
		return unavailable("file is empty")
	}
	return checkedSecret(value, "file:"+configured)
}

// readSecret enforces the bound independently of stat, including growing files.
// Its errors are public-safe; reader errors must not escape with file names.
func readSecret(ctx context.Context, reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: reader}, (64<<10)+1))
	if err != nil {
		return nil, errors.New("file is unreadable")
	}
	if len(data) > 64<<10 {
		return nil, errors.New("file exceeds 64 KiB")
	}
	return data, nil
}

// quote returns a TOML basic string.
func quote(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// contextReader keeps cancellation checks at each bounded read operation.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
