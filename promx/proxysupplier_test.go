package promx

import (
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/proxysupplier"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestWebshareDirectCollectorsExposeConsistentPoolSnapshot(t *testing.T) {
	Init("go-test-service", "test")
	reg := prometheus.NewRegistry()
	c := NewWebshareDirectCollectors(reg)
	c.snapshot = func() proxysupplier.WebshareDirectPoolState {
		return proxysupplier.WebshareDirectPoolState{
			Total:      5,
			InCooldown: 2,
			Eligible:   3,
		}
	}

	want := `
# HELP proxysupplier_webshare_direct_pool_eligible Current proxy IPs eligible for selection by the webshare_direct supplier.
# TYPE proxysupplier_webshare_direct_pool_eligible gauge
proxysupplier_webshare_direct_pool_eligible{service="go-test-service"} 3
# HELP proxysupplier_webshare_direct_pool_in_cooldown Current proxy IPs excluded by the webshare_direct supplier because their cooldown has not elapsed.
# TYPE proxysupplier_webshare_direct_pool_in_cooldown gauge
proxysupplier_webshare_direct_pool_in_cooldown{service="go-test-service"} 2
# HELP proxysupplier_webshare_direct_pool_size Total proxy IPs loaded by the webshare_direct supplier from its last successful catalog refresh.
# TYPE proxysupplier_webshare_direct_pool_size gauge
proxysupplier_webshare_direct_pool_size{service="go-test-service"} 5
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"proxysupplier_webshare_direct_pool_size",
		"proxysupplier_webshare_direct_pool_in_cooldown",
		"proxysupplier_webshare_direct_pool_eligible",
	); err != nil {
		t.Fatal(err)
	}
}

func TestWebshareDirectCollectorsExposeZeroWhenDisabled(t *testing.T) {
	Init("go-disabled-service", "test")
	reg := prometheus.NewRegistry()
	c := NewWebshareDirectCollectors(reg)
	c.snapshot = func() proxysupplier.WebshareDirectPoolState {
		return proxysupplier.WebshareDirectPoolState{}
	}

	want := `
# HELP proxysupplier_webshare_direct_pool_eligible Current proxy IPs eligible for selection by the webshare_direct supplier.
# TYPE proxysupplier_webshare_direct_pool_eligible gauge
proxysupplier_webshare_direct_pool_eligible{service="go-disabled-service"} 0
# HELP proxysupplier_webshare_direct_pool_in_cooldown Current proxy IPs excluded by the webshare_direct supplier because their cooldown has not elapsed.
# TYPE proxysupplier_webshare_direct_pool_in_cooldown gauge
proxysupplier_webshare_direct_pool_in_cooldown{service="go-disabled-service"} 0
# HELP proxysupplier_webshare_direct_pool_size Total proxy IPs loaded by the webshare_direct supplier from its last successful catalog refresh.
# TYPE proxysupplier_webshare_direct_pool_size gauge
proxysupplier_webshare_direct_pool_size{service="go-disabled-service"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"proxysupplier_webshare_direct_pool_size",
		"proxysupplier_webshare_direct_pool_in_cooldown",
		"proxysupplier_webshare_direct_pool_eligible",
	); err != nil {
		t.Fatal(err)
	}
}
