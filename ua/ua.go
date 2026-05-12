package ua

import "fmt"

// Build returns the standard 0crawl branded User-Agent string.
// Format: "<serviceID>/<version> (+https://github.com/baditaflorin/<serviceID>)"
func Build(serviceID, version string) string {
	return fmt.Sprintf("%s/%s (+https://github.com/baditaflorin/%s)", serviceID, version, serviceID)
}
