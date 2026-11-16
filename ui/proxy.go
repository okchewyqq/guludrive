package workspace

import (
	"context"

	"guludrive/ui/api"
)

func (a *uiApp) stopProxyWork() {
	a.proxyEpoch++
	if a.proxyCancel != nil {
		a.proxyCancel()
	}
	a.proxyCancel = nil
	a.scriptBusy = false
}
func (a *uiApp) copyWorkerScript() {
	if a.busy || a.scriptBusy || !a.connected || a.client == nil || a.dispatch == nil || a.ctx.Err() != nil || a.page != "Settings" {
		return
	}
	a.stopProxyWork()
	epoch, client, origin, password := a.proxyEpoch, a.client, a.publicOrigin, a.workerSecret
	ctx, cancel := context.WithCancel(a.ctx)
	a.proxyCancel = cancel
	a.scriptBusy = true
	go func() {
		script, err := client.GenerateProxyScript(ctx, origin, password)
		cancelled := ctx.Err() != nil
		cancel()
		a.dispatch(func() {
			defer func() { script = "" }()
			if epoch != a.proxyEpoch || cancelled || a.ctx.Err() != nil || !a.connected || a.client != client || a.page != "Settings" {
				return
			}
			a.proxyCancel = nil
			a.scriptBusy = false
			if err != nil {
				if api.Unauthorized(err) {
					a.clearSession()
				}
				a.fail(err)
				return
			}
			a.copy(script, "Worker script")
		})
	}()
}
