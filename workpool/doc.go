// Package workpool provides a bounded-concurrency worker pool backed by
// a semaphore and a WaitGroup.
//
// Usage:
//
//	pool := workpool.New("ingester", 8) // max 8 concurrent tasks
//	for _, item := range items {
//	    item := item
//	    pool.Submit(func() { process(item) })
//	}
//	pool.Wait()
package workpool
