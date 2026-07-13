package main

import (
	"os"

	"github.com/golovanov-dev/traceloom-go/internal/eventtracecli"
)

func main() {
	command := eventtracecli.Command{Out: os.Stdout, Err: os.Stderr}
	os.Exit(command.Run(os.Args[1:]))
}
