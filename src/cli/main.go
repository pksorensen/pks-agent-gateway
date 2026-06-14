package main

import (
	"os"

	"github.com/pksorensen/pks-agent-gateway/src/cli/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
