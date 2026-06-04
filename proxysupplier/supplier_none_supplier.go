package proxysupplier

type noneSupplier struct{}

func (noneSupplier) Name() string { return "none" }

func (noneSupplier) ProxyURL() string { return "" }
