package version

import "runtime"

func goVersion() string { return runtime.Version() }
