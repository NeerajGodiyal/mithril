package main

import (
	"os"

	"github.com/Overclock-Validator/mithril/pkg/tpu/quicserver/testutils"
)

func main() {
	os.Exit(testutils.MainBench(os.Args[1:]))
}
