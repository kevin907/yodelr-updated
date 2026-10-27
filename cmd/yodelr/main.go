// Command yodelr starts the interactive Yodelr CLI.
//
// Usage:
//
//	go run ./cmd/yodelr                   # interactive session
//	go run ./cmd/yodelr < commands.txt    # run a script of commands
package main

import (
	"os"

	"github.com/kevin907/yodelr-updated/cli"
)

func main() {
	info, err := os.Stdin.Stat()
	interactive := err == nil && info.Mode()&os.ModeCharDevice != 0
	cli.New(os.Stdout).Run(os.Stdin, interactive)
}
