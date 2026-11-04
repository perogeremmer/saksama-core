package main

import (
	"os"

	"github.com/perogeremmer/saksama-core/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
