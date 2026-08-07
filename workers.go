package hfs

import (
	"context"
	"sync"
)

// parallelMap runs fn over each index in [0, n) using at most workers
// goroutines, and returns the results in index order.
//
// # Determinism
//
// Results are written into a preallocated slice at their own index, never
// appended, so the output is identical for any worker count — including one.
// Nothing is shared between tasks: fn receives an index and returns a value,
// and the only cross-goroutine state is the error/cancellation signal. This is
// what lets carving be parallel without making its findings depend on
// scheduling.
//
// # Supervision
//
// Every goroutine is joined before returning. Cancellation is cooperative: the
// context is checked between tasks, so a task already running finishes rather
// than being abandoned mid-read. The first non-nil error wins and cancels the
// rest; later errors are discarded, since one cause is enough to explain the
// failure.
//
// workers < 2 runs everything on the calling goroutine with no goroutines
// spawned at all — the degenerate case is genuinely sequential, not a pool of
// size one.
func parallelMap[T any](ctx context.Context, workers, n int, fn func(ctx context.Context, i int) (T, error)) ([]T, error) {
	out := make([]T, n)
	if n == 0 {
		return out, nil
	}

	if workers < 2 || n == 1 {
		for i := range n {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			v, err := fn(ctx, i)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}

	if workers > n {
		workers = n
	}

	// A derived context lets the first failure stop the remaining workers
	// without disturbing the caller's context.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	tasks := make(chan int)

	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range tasks {
				if err := runCtx.Err(); err != nil {
					fail(err)
					return
				}
				v, err := fn(runCtx, i)
				if err != nil {
					fail(err)
					return
				}
				out[i] = v // distinct index per task: no synchronisation needed
			}
		}()
	}

	// Feeding from the caller's goroutine keeps the fan-out bounded: the send
	// blocks until a worker is free, so no task queue can grow without limit.
feed:
	for i := range n {
		select {
		case tasks <- i:
		case <-runCtx.Done():
			break feed
		}
	}
	close(tasks)
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// blockRange is one unit of carving work: a span of allocation blocks to scan.
type blockRange struct {
	start uint32
	count uint32
}

// splitBlockRuns divides free-space runs into batches of at most batchSize
// blocks, so work is spread evenly across workers regardless of how the free
// space happens to be laid out.
//
// A single run covering most of the volume — the common case on a
// lightly-used image — would otherwise become one task and defeat the pool.
func splitBlockRuns(runs []blockRange, batchSize uint32) []blockRange {
	if batchSize == 0 {
		return runs
	}
	out := make([]blockRange, 0, len(runs))
	for _, r := range runs {
		for off := uint32(0); off < r.count; off += batchSize {
			n := batchSize
			if remaining := r.count - off; remaining < n {
				n = remaining
			}
			out = append(out, blockRange{start: r.start + off, count: n})
		}
	}
	return out
}
