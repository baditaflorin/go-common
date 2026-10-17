package ua_test

import (
	"fmt"

	"github.com/baditaflorin/go-common/ua"
)

func ExampleBuild() {
	fmt.Println(ua.Build("go_example", "v1.2.3"))
	// Output: go_example/v1.2.3 (+https://github.com/baditaflorin/go_example)
}
