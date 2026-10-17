package wordbound_test

import (
	"fmt"

	"github.com/baditaflorin/go-common/wordbound"
)

// Go's regexp \b only knows ASCII, so it cannot tell that "АО" sits inside the
// Cyrillic word "ПАО". ContainsToken can.
func ExampleContainsToken() {
	fmt.Println(wordbound.ContainsToken("Acme Inc", "Inc"))
	fmt.Println(wordbound.ContainsToken("Income statement", "Inc"))
	fmt.Println(wordbound.ContainsToken("ПАО Газпром", "АО"))
	// Output:
	// true
	// false
	// false
}
