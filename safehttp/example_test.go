package safehttp_test

import (
	"fmt"
	"net"
	"time"

	"github.com/baditaflorin/go-common/safehttp"
	"github.com/baditaflorin/go-common/ua"
)

// NewClient is the client for the PUBLIC internet: it refuses loopback,
// private, link-local and CGNAT targets, again at connect time, so a DNS name
// that rebinds to 169.254.169.254 is stopped too. Calls to sibling fleet
// services on the private mesh must use a plain net/http.Client instead.
func ExampleNewClient() {
	client := safehttp.NewClient(
		safehttp.WithTimeout(10*time.Second),
		safehttp.WithUserAgent(ua.Build("go_example", "v1.0.0")),
	)
	_ = client // client.Get("https://example.com") ...
}

func ExampleNormalizeURL() {
	u, err := safehttp.NormalizeURL("  example.com/path ")
	fmt.Println(u, err)
	_, err = safehttp.NormalizeURL("ftp://example.com")
	fmt.Println(err)
	// Output:
	// https://example.com/path <nil>
	// scheme must be http or https
}

func ExampleIsBlocked() {
	fmt.Println(safehttp.IsBlocked(net.ParseIP("10.0.0.5")))
	fmt.Println(safehttp.IsBlocked(net.ParseIP("169.254.169.254")))
	fmt.Println(safehttp.IsBlocked(net.ParseIP("8.8.8.8")))
	// Output:
	// true
	// true
	// false
}
