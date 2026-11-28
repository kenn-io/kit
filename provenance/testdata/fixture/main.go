package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "unstamped":
		fmt.Print(`{}`)
	case "output":
		fmt.Print(os.Args[2])
	case "large":
		fmt.Print(strings.Repeat("x", 17<<10))
	case "fail":
		fmt.Fprint(os.Stderr, "fixture failed")
		os.Exit(1)
	}
}
