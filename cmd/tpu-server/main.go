package main

import (
	"os"

	"github.com/Overclock-Validator/mithril/pkg/tpu/quicserver/testutils"
)

func main() {
	os.Exit(testutils.MainServer(os.Args[1:]))
}
