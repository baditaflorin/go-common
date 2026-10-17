// Command compatcheck reports whether a service's go.mod pins a go-common
// version that is missing a security fix go-common shipped.
//
//	go run github.com/baditaflorin/go-common/scripts/compatcheck@latest -gomod go.mod
//
// Running it with @latest uses the newest manifest, which is the point: the floor
// moves forward as fixes ship, and a pinned copy of this tool would not know.
// Exit status: 0 ok (or not applicable), 1 below the floor, 2 could not check
// (`go run` reports every failure as 1; use a built binary to tell them apart).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/baditaflorin/go-common/compat"
)

func main() {
	goMod := flag.String("gomod", "go.mod", "path to the go.mod to check")
	asJSON := flag.Bool("json", false, "print the result as JSON")
	flag.Parse()

	data, err := os.ReadFile(*goMod)
	if err != nil {
		fmt.Fprintln(os.Stderr, "compatcheck:", err)
		os.Exit(2)
	}
	m, err := compat.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "compatcheck:", err)
		os.Exit(2)
	}
	res, err := m.CheckGoMod(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "compatcheck:", err)
		os.Exit(2)
	}

	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(res)
	} else {
		fmt.Printf("compatcheck: %s: %s\n", res.Status, res.Message)
	}
	if res.Status == compat.StatusBelowFloor {
		os.Exit(1)
	}
}
