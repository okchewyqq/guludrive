package workspace

import (
	"context"
	"fmt"
	"strings"

	"guludrive/ui/api"
	"guludrive/ui/i18n"

	"github.com/egoist/mygo/ui"
)

func (a *uiApp) copyDownloadLink(e api.Entry) {
	if a.busy || !a.connected || e.IsDir {
		return
	}
	c := a.client
	a.launch("Copy download link", func(ctx context.Context) (func(), error) {
		link, err := c.CreateDownloadLink(ctx, e.Path)
		if err != nil {
			return nil, err
		}
		return func() { a.copy(link.URL, "Download link") }, nil
	})
}

func (a *uiApp) copyDownloadLinks() {
	entries := a.selectedFiles()
	if a.busy || !a.connected || len(entries) == 0 {
		return
	}
	if len(entries) == 1 {
		a.copyDownloadLink(entries[0])
		return
	}
	c := a.client
	a.launch("Copy download links", func(ctx context.Context) (func(), error) {
		links := make([]string, 0, len(entries))
		for _, e := range entries {
			link, err := c.CreateDownloadLink(ctx, e.Path)
			if err != nil {
				return nil, err
			}
			links = append(links, link.URL)
		}
		text := strings.Join(links, "\n")
		return func() { a.copyDownloadText(text, len(links)) }, nil
	})
}

func (a *uiApp) copyDownloadText(value string, count int) {
	a.copy(value, "Download link")
	if a.notice == "" || count == 1 {
		return
	}
	a.notice = "Copied %d download links"
	a.noticeArgs = []any{count}
}

func downloadModeLabel(mode api.DownloadMode) string {
	switch mode {
	case api.DownloadLocal:
		return "Local"
	case api.DownloadGDirect:
		return "GDirect"
	case api.DownloadProxy:
		return "CF Proxy"
	}
	return "Not connected"
}
func (a *uiApp) clearWorkerSecret() { a.workerSecret = ""; a.secretEpoch++ }
func (a *uiApp) resetDownloadDraft() {
	a.downloadMode, a.workerURL, a.publicOrigin = a.snapshot.Downloads.Mode, a.snapshot.Downloads.WorkerURL, a.snapshot.Downloads.PublicOrigin
	a.clearWorkerSecret()
}
func (a *uiApp) chooseDownloadMode(mode api.DownloadMode) {
	if a.busy || !a.connected || !mode.Valid() {
		return
	}
	a.downloadMode = mode
	a.err, a.notice = "", ""
	a.currentError, a.noticeArgs = nil, nil
	if mode == api.DownloadProxy && (!a.snapshot.Downloads.WorkerConfigured || !a.snapshot.Downloads.PublicOriginReady) {
		return
	}
	if mode != a.snapshot.Downloads.Mode {
		a.saveDownloadSettings()
	}
}
func (a *uiApp) saveDownloadSettings() {
	if a.busy || !a.connected || a.client == nil || a.dispatch == nil || a.ctx.Err() != nil {
		return
	}
	c := a.client
	input := api.DownloadSettingsUpdate{Mode: a.downloadMode, WorkerURL: a.workerURL, WorkerSecret: a.workerSecret, PublicOrigin: a.publicOrigin}
	if input.Mode != api.DownloadProxy {
		input.WorkerSecret = ""
	}
	if input.Mode == api.DownloadProxy && !a.snapshot.Downloads.WorkerConfigured && input.WorkerSecret == "" {
		a.fail(i18n.Err("Enter the proxy password to activate CF Proxy"))
		return
	}
	a.stopGDirectPoll()
	a.downloadApplying = input.Mode
	label := "Save download settings"
	if input.Mode == api.DownloadGDirect {
		label = "Selecting GDirect IP"
	}
	a.launch(label, func(ctx context.Context) (func(), error) {
		settings, err := c.UpdateDownloadSettings(ctx, input)
		var status api.Status
		var statusErr error
		if err == nil {
			status, statusErr = c.Status(ctx)
		}
		result := err
		if result == nil {
			result = statusErr
		}
		return func() {
			a.downloadApplying = ""
			if err == nil {
				a.snapshot.Downloads = settings
				a.downloadMode, a.workerURL, a.publicOrigin = settings.Mode, settings.WorkerURL, settings.PublicOrigin
				a.clearWorkerSecret()
				if statusErr == nil {
					a.snapshot.Status = status
				} else {
					a.snapshot.Status.GDirect = api.GDirectStatus{Enabled: settings.Mode == api.DownloadGDirect}
				}
				a.notice = "Download settings saved"
			}
			// A failed mode change leaves the previous mode active, with the draft visible.
			a.startGDirectPoll()
		}, result
	})
}
func (a *uiApp) refreshGDirect() {
	if a.busy || !a.connected || a.client == nil || a.dispatch == nil || a.ctx.Err() != nil || a.snapshot.Downloads.Mode != api.DownloadGDirect {
		return
	}
	c := a.client
	a.stopGDirectPoll()
	a.gdirectRefreshing = true
	a.launch("Refresh GDirect IP", func(ctx context.Context) (func(), error) {
		status, err := c.RefreshGDirect(ctx)
		return func() {
			a.gdirectRefreshing = false
			if err == nil {
				a.snapshot.Status.GDirect = status
			}
			a.startGDirectPoll()
		}, err
	})
}
func (a *uiApp) gdirectSelecting() bool {
	return a.downloadApplying == api.DownloadGDirect || a.gdirectRefreshing || a.snapshot.Status.GDirect.Selecting
}
func (a *uiApp) gdirectText() string {
	if a.gdirectSelecting() {
		return a.t("Selecting verified IP…")
	}
	if ip := a.snapshot.Status.GDirect.IP; ip != "" {
		return ip
	}
	return a.t("No verified IP available")
}
func (a *uiApp) downloadSettingsView(c *ui.Context, p palette) {
	a.settingsCard(c, p, "Download links", func() {
		ui.Row(c).Gap(6).Wrap().Children(func() {
			for _, mode := range []api.DownloadMode{api.DownloadLocal, api.DownloadGDirect, api.DownloadProxy} {
				kind := "outline"
				if a.downloadMode == mode {
					kind = "tonal"
				}
				a.button(c, "download-mode:"+string(mode), downloadModeLabel(mode), nil, kind, a.busy).AccessSelected(a.downloadMode == mode).OnClick(func() { a.chooseDownloadMode(mode) })
			}
		})
		ui.Column(c).Gap(4).Padding(8, 0).Children(func() {
			ui.Text(c, a.t("Active mode")).FontSize(11).TextColor(p.muted)
			ui.Text(c, a.t(downloadModeLabel(a.snapshot.Downloads.Mode))).FontSize(16).FontWeight(600)
		})
		description := map[api.DownloadMode]string{
			api.DownloadGDirect: "Selected IP relay",
			api.DownloadProxy:   "Cloudflare relay",
		}[a.downloadMode]
		if description != "" {
			ui.Text(c, a.t(description)).FontSize(12).TextColor(p.muted)
		}
		if a.downloadMode == api.DownloadGDirect || a.snapshot.Downloads.Mode == api.DownloadGDirect {
			ui.Row(c).Gap(12).Wrap().Children(func() {
				if a.gdirectSelecting() {
					ui.Spinner(c).Label(a.t("Selecting verified IP…"))
				}
				ui.Text(c, a.gdirectText()).FontSize(13).TextColor(p.primary).Grow(1).MinWidth(0).Selectable().Tooltip(a.t("Verified selected runtime IP; not saved and reselected after restart."))
				a.button(c, "refresh-gdirect", "Refresh IP", iRefresh, "outline", a.busy || a.snapshot.Downloads.Mode != api.DownloadGDirect).OnClick(a.refreshGDirect)
			})
		}
		if a.downloadMode != a.snapshot.Downloads.Mode {
			ui.Text(c, a.t("Draft mode · %s", a.t(downloadModeLabel(a.downloadMode)))).FontSize(12).TextColor(p.muted)
		}
		if a.downloadMode == api.DownloadProxy {
			a.field(c, "public-origin", "Server public address", &a.publicOrigin, false).Disabled(a.busy)
			a.field(c, fmt.Sprintf("worker-secret:%d", a.secretEpoch), "Proxy password", &a.workerSecret, true).Disabled(a.busy)
			a.button(c, "copy-worker-script", "Copy Worker script", iCopy, "outline", a.busy || a.scriptBusy).AlignSelf(ui.Start).OnClick(a.copyWorkerScript)
			a.field(c, "worker-url", "Worker address", &a.workerURL, false).Disabled(a.busy)
			if strings.HasPrefix(strings.ToLower(a.publicOrigin), "http://") || strings.HasPrefix(strings.ToLower(a.workerURL), "http://") {
				ui.Text(c, a.t("HTTP is unencrypted; HTTPS is recommended.")).FontSize(12).TextColor(p.muted)
			}
			a.button(c, "save-download-settings", "Save and activate CF Proxy", nil, "tonal", a.busy || a.scriptBusy).AlignSelf(ui.Start).OnClick(a.saveDownloadSettings)
		}
	})
}
func (a *uiApp) gdirectBadge(c *ui.Context, p palette, width float32) {
	if !a.connected || !a.snapshot.Status.GDirect.Enabled {
		return
	}
	label := "GDirect · " + a.t("Current selected IP") + " · " + a.gdirectText()
	maxWidth := float32(230)
	if width < 760 {
		maxWidth = 138
	}
	ui.Text(c, label).MaxWidth(maxWidth).MinWidth(0).SingleLine().Tooltip(a.t("Verified selected runtime IP; not saved and reselected after restart.")+" · "+a.gdirectText()).FontSize(11).TextColor(p.primary).Background(p.primaryContainer).Radius(controlRadius).Padding(6, 8)
}
