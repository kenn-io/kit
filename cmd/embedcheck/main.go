// Command embedcheck checks a service against the Kit embedding HTTP contract.
package main

import (
	"context"
	"flag"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

type paths []string

func (p *paths) String() string     { return strings.Join(*p, ",") }
func (p *paths) Set(s string) error { *p = append(*p, s); return nil }

func main() { os.Exit(run()) }
func run() int {
	var opts embedclient.CheckOptions
	var media paths
	var keyEnv string
	flag.StringVar(&opts.BaseURL, "url", "", "service base URL ending in /v1")
	flag.StringVar(&opts.Model, "model", "", "model name")
	flag.IntVar(&opts.Dimensions, "dims", 0, "vector dimensions")
	flag.StringVar(&keyEnv, "api-key-env", "", "environment variable containing the API key")
	flag.Var(&media, "media", "media file the service must embed; repeat for each file")
	flag.Parse()
	if opts.BaseURL == "" || opts.Model == "" || opts.Dimensions <= 0 || flag.NArg() != 0 {
		flag.Usage()
		return 2
	}
	if keyEnv != "" {
		var ok bool
		opts.APIKey, ok = os.LookupEnv(keyEnv)
		if !ok {
			fmt.Fprintln(os.Stderr, "API key environment variable is unset")
			return 2
		}
	}
	for _, path := range media {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		typ := mime.TypeByExtension(filepath.Ext(path))
		if typ == "" {
			fmt.Fprintln(os.Stderr, "unknown media type:", path)
			return 2
		}
		opts.Media = append(opts.Media, embedclient.Input{Role: embedconfig.RoleDocument, MediaType: typ, Data: data})
	}
	failures := embedclient.Check(context.Background(), opts)
	for _, err := range failures {
		fmt.Fprintln(os.Stderr, err)
	}
	if len(failures) > 0 {
		return 1
	}
	return 0
}
