package reqstats

import (
	"runtime"
	"syscall"
	"time"
)

// procCPU returns cumulative process CPU time (user+system) via getrusage.
// RUSAGE_SELF is process-wide, so a delta over a request window is
// over-attributed under concurrency — this feeds the labeled `approx` block
// only. int64() conversions keep it portable across linux (Usec int64) and
// darwin (Usec int32). Returns 0 if the syscall is unavailable.
func procCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(int64(ru.Utime.Usec))*time.Microsecond
	sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(int64(ru.Stime.Usec))*time.Microsecond
	return user + sys
}

// heapAlloc returns cumulative bytes allocated by the process. The delta over a
// request is also process-wide (concurrency-contaminated). runtime.ReadMemStats
// is a small stop-the-world, so this is opt-in (EnableHeapDelta).
func heapAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}
