// Package bootgate is an opt-in primitive for "do not start heavy boot-time
// work all at once": it spreads a service's first heavy cycle out in time and
// backs off while the host is overloaded.
//
// # Why this exists
//
// On 2026-10-11 a Docker daemon restart brought ~330 containers up in the same
// instant (load average 468, daemon unresponsive for minutes), and a backup
// service that began a multi-GiB job at every boot starved its own HTTP
// handlers inside a deploy's 6 s smoke window. With 300+ services on one
// fleet, nothing heavy should ever start all at once; start in small
// incremental batches instead.
//
// # What it does
//
// Wait blocks, in order, for:
//
//  1. BaseDelay (a fixed head start for everything else on the host);
//  2. a DETERMINISTIC jitter in [0, JitterMax), derived from an FNV-1a hash
//     of Seed (default: the container hostname, falling back to Name). The
//     same container always gets the same offset across restarts, and a
//     fleet of distinct hostnames spreads evenly over the window instead of
//     clumping the way independent random draws do;
//  3. if MaxLoad > 0, the host 1-minute load average (/proc/loadavg) to fall
//     below MaxLoad, polling every PollEvery, for at most MaxWait. The gate
//     never blocks forever: on timeout Wait returns Result.TimedOut = true
//     and a nil error, and the caller proceeds. Where the load average is
//     unavailable (darwin, Windows, unreadable /proc) the gate is skipped and
//     Result.LoadKnown is false.
//
// Wait returns ctx.Err() promptly if the context is cancelled.
//
// # Nothing changes unless configured
//
// Every Config field defaults to zero/disabled, and FromEnv reads only the
// BOOT_* variables below. Importing the package, or calling Wait with an
// unconfigured service, returns immediately. A service opts in by calling
// Wait AND an operator opting in by setting env.
//
//	BOOT_BASE_DELAY   duration, e.g. 0s
//	BOOT_JITTER_MAX   duration, e.g. 180s
//	BOOT_MAX_LOAD     float, 1-minute load average ceiling, e.g. 6 on 8 cores
//	BOOT_MAX_WAIT     duration, cap on the load gate (default 5m when MaxLoad>0)
//	BOOT_POLL_EVERY   duration, load poll interval (default 5s)
//
// Invalid or negative values fall back to the disabled default.
//
// Recommended fleet values for heavy workers (backup, indexing, rebuilds):
// BOOT_BASE_DELAY=0, BOOT_JITTER_MAX between 60s and 300s, BOOT_MAX_LOAD about
// 0.75 x cores, BOOT_MAX_WAIT=10m. Light, latency-sensitive services should
// leave it unset.
//
// # Usage
//
// A backup-style ticker gates only its first heavy cycle:
//
//	func run(ctx context.Context) {
//	    res, err := bootgate.Wait(ctx, bootgate.FromEnv("fleet-backup"))
//	    if err != nil {
//	        return // ctx cancelled while waiting
//	    }
//	    log.Printf("fleet-backup: boot gate waited=%s jitter=%s load=%.2f timed_out=%t",
//	        res.Waited, res.Jitter, res.LoadAtEnd, res.TimedOut)
//
//	    t := time.NewTicker(interval)
//	    defer t.Stop()
//	    for {
//	        backupCycle(ctx)
//	        select {
//	        case <-ctx.Done():
//	            return
//	        case <-t.C:
//	        }
//	    }
//	}
//
// Or, to run a function in the background after the gate, logging one line:
//
//	bootgate.Go(ctx, bootgate.FromEnv("fleet-backup"), runBackups)
//
// # Rollout
//
// go-common is shared by ~130 services. Adopt this in small batches (a few
// heavy workers at a time), and enable the env knobs per service, not
// fleet-wide in one step.
package bootgate
