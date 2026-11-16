package store

import (
	"context"
	"sync"

	"guludrive/core"
)

// PrepareDownload pins the group while resolving every original segment URL.
// No network operation holds stateMu or a SQL transaction; no body is read.
func (s *Store) PrepareDownload(ctx context.Context, p string) (core.DownloadSource, error) {
	p, err := core.NormalizePath(p)
	if err != nil {
		return core.DownloadSource{}, err
	}
	ctx, done, err := s.start(ctx)
	if err != nil {
		return core.DownloadSource{}, err
	}
	defer done()
	n, o, err := s.pinRead(ctx, p)
	if err != nil {
		return core.DownloadSource{}, err
	}
	defer func() { s.stateMu.Lock(); s.unpinLocked(o.id); s.stateMu.Unlock() }()
	if err = s.readBounds(n, o); err != nil {
		return core.DownloadSource{}, err
	}
	out := core.DownloadSource{Entry: n.Entry, Segments: make([]core.DownloadSegment, len(o.manifest.Parts))}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	var first sync.Once
	var firstErr error
	fail := func(err error) {
		first.Do(func() { firstErr = safeError(err, "prepare download"); cancel() })
	}
	for range min(4, len(o.manifest.Parts)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if workCtx.Err() != nil {
					return
				}
				part := o.manifest.Parts[index]
				url, err := s.cfg.Backend.DownloadURL(workCtx, part.Remote)
				if err != nil {
					fail(err)
					return
				}
				if err = workCtx.Err(); err != nil {
					fail(err)
					return
				}
				if url == "" {
					fail(core.ErrInvalid)
					return
				}
				out.Segments[index] = core.DownloadSegment{Offset: part.Plan.Offset, Size: part.Plan.Length, URL: url, PayloadOffset: part.Carrier.PayloadOffset, CarrierSize: part.Carrier.Size}
			}
		}()
	}
dispatch:
	for index := range o.manifest.Parts {
		select {
		case <-workCtx.Done():
			break dispatch
		case jobs <- index:
		}
	}
	close(jobs)
	// A canceled transport may still be unwinding. Keep the group pinned until
	// every worker exits, and prefer its first safe error over child cancellation.
	workers.Wait()
	if firstErr != nil {
		return core.DownloadSource{}, firstErr
	}
	if err = ctx.Err(); err != nil {
		return core.DownloadSource{}, err
	}
	return out, nil
}
