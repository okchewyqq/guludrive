package workspace

import (
	"context"
	"errors"
	"time"

	"guludrive/ui/api"
)

// Poll only an in-progress server selection. Both the timer and HTTP request
// share a bounded, cancellable context; the client never chooses or stores IPs.
func pollGDirect(ctx context.Context, c *api.Client, status api.GDirectStatus, interval time.Duration, update func(api.GDirectStatus)) error {
	for status.Enabled && status.Selecting {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		latest, err := c.Status(ctx)
		if err != nil {
			return err
		}
		status = latest.GDirect
		update(status)
	}
	return nil
}
func (a *uiApp) stopGDirectPoll() {
	a.gdirectEpoch++
	if a.gdirectPollCancel != nil {
		a.gdirectPollCancel()
	}
	a.gdirectPollCancel = nil
}
func (a *uiApp) startGDirectPoll() {
	a.stopGDirectPoll()
	status := a.snapshot.Status.GDirect
	if !a.connected || a.client == nil || a.dispatch == nil || a.ctx.Err() != nil || a.snapshot.Downloads.Mode != api.DownloadGDirect || !status.Enabled || !status.Selecting {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	a.gdirectPollCancel = cancel
	c, epoch := a.client, a.gdirectEpoch
	current := func() bool { return a.connected && a.client == c && a.gdirectEpoch == epoch && a.ctx.Err() == nil }
	go func() {
		defer cancel()
		err := pollGDirect(ctx, c, status, time.Second, func(status api.GDirectStatus) {
			a.dispatch(func() {
				if current() {
					a.snapshot.Status.GDirect = status
				}
			})
		})
		a.dispatch(func() {
			if !current() {
				return
			}
			a.gdirectPollCancel = nil
			if errors.Is(err, context.DeadlineExceeded) {
				a.notice = "IP selection is still in progress. Refresh the workspace to check again."
				a.noticeArgs = nil
			} else if err != nil && !errors.Is(err, context.Canceled) {
				a.fail(err)
				if api.Unauthorized(err) {
					a.clearSession()
				}
			}
		})
	}()
}
