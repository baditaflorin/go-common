package promx_test

import (
	"fmt"

	"github.com/baditaflorin/go-common/promx"
)

// A BoundedLabel names the values that recur and counts the rest under one value,
// so the label takes a fixed number of values whatever it is fed. Here two slots
// go to the first two hosts seen twice.
func ExampleBoundedLabel() {
	hosts := promx.NewBoundedLabel(2, promx.WithMinHits(2))
	for _, h := range []string{"a.example", "a.example", "b.example", "c.example", "c.example", "d.example", "d.example", ""} {
		fmt.Println(hosts.Value(h))
	}
	// Output:
	// _other
	// a.example
	// _other
	// _other
	// c.example
	// _other
	// _other
	// _unknown
}
