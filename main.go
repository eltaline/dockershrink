package main

import (
	"os"

	"github.com/eltaline/dockershrink/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
