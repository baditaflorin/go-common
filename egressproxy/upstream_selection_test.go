package egressproxy

import (
	"testing"

	"github.com/baditaflorin/go-common/proxysupplier"
)

func TestMarkResultReportsExactWebshareSelection(t *testing.T) {
	selection := proxysupplier.WebshareSelection{PoolID: "dc-quality", AccountID: "quality", Class: proxysupplier.PoolClassDC}
	supplier := &selectionReporterSupplier{selection: selection}
	p := &Proxy{cfg: Config{Supplier: supplier}}
	p.markResult(&upstream{addr: "192.0.2.10:8000", selection: &selection}, false)
	if len(supplier.marked) != 1 || supplier.marked[0].PoolID != "dc-quality" || len(supplier.results) != 1 || supplier.results[0] {
		t.Fatalf("typed result report = selections:%v results:%v, want dc-quality failure", supplier.marked, supplier.results)
	}
}
