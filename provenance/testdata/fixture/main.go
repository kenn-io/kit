package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"go.kenn.io/kit/provenance"
)

var version, commit, buildTime, sourceTime, distribution string

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "output":
			fmt.Print(os.Args[2])
			return
		case "fail":
			fmt.Fprint(os.Stderr, "fixture failed")
			os.Exit(1)
		}
	}
	parse := func(value string) time.Time {
		if value == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(1)
		}
		return t
	}
	_ = json.NewEncoder(os.Stdout).Encode(provenance.Info{SchemaVersion: provenance.SchemaVersion, Version: version, Commit: commit, BuildTime: parse(buildTime), SourceTime: parse(sourceTime), Distribution: distribution})
}
