package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Overclock-Validator/mithril/pkg/turbine"
)

func main() {
	fixturePath := flag.String("fixture", "agave/turbine-vector/fixtures/default.json", "path to shared fixture JSON")
	flag.Parse()

	fixture, err := turbine.LoadVectorFixture(*fixturePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load fixture: %v\n", err)
		os.Exit(1)
	}
	out, err := turbine.EmitVector(fixture, "mithril")
	if err != nil {
		fmt.Fprintf(os.Stderr, "emit vector: %v\n", err)
		os.Exit(1)
	}
	os.Stdout.Write(out)
	os.Stdout.Write([]byte("\n"))
}
