package workspace

import (
	"context"
	"strconv"

	"guludrive/ui/api"
	"guludrive/ui/i18n"
	"guludrive/ui/state"

	"github.com/egoist/mygo/ui"
)

type uploadSettingsForm struct {
	Confirmed               api.UploadSettings
	Format, MP4GB, BMPMB    string
	Loaded, Loading, Saving bool
	LoadError, SaveError    error
	Notice                  string
	epoch                   uint64
	cancel                  context.CancelFunc
}

func (f *uploadSettingsForm) resetDraft() {
	// Defaults, like all persisted policy, come from a successful server GET.
	f.Format, f.MP4GB, f.BMPMB = "", "", ""
	if f.Confirmed.LargeFileFormat != "" {
		f.Format = f.Confirmed.LargeFileFormat
		f.MP4GB = strconv.FormatFloat(float64(f.Confirmed.MP4PartSize)/1_000_000_000, 'f', -1, 64)
		f.BMPMB = strconv.FormatFloat(float64(f.Confirmed.BMPPartSize)/1_000_000, 'f', -1, 64)
	}
}
func (a *uiApp) stopUploadSettings() {
	f := &a.uploadSettings
	f.epoch++
	if f.cancel != nil {
		f.cancel()
	}
	f.cancel = nil
	f.Loading, f.Saving = false, false
	f.LoadError, f.SaveError, f.Notice = nil, nil, ""
	f.resetDraft()
}
func (a *uiApp) loadUploadSettings() {
	if !a.connected || a.client == nil || a.dispatch == nil || a.ctx.Err() != nil || a.page != "Settings" {
		return
	}
	a.stopUploadSettings()
	a.uploadSettings.Loaded = false
	a.requestUploadSettings(nil)
}
func (a *uiApp) saveUploadSettings() {
	f := &a.uploadSettings
	if a.busy || !a.connected || a.client == nil || a.dispatch == nil || a.ctx.Err() != nil || a.page != "Settings" || !f.Loaded || f.Loading || f.Saving {
		return
	}
	f.SaveError, f.Notice = nil, ""
	input, err := state.ParseUploadSettings(f.Format, f.MP4GB, f.BMPMB)
	if err != nil {
		f.SaveError = err
		return
	}
	a.requestUploadSettings(&input)
}

// This card has its own request state; saving must not reload the workspace or
// clear either the input-only Worker secret or administrator password editors.
func (a *uiApp) requestUploadSettings(input *api.UploadSettings) {
	f := &a.uploadSettings
	f.epoch++
	epoch, client := f.epoch, a.client
	ctx, cancel := context.WithCancel(a.ctx)
	f.cancel = cancel
	f.Loading, f.Saving = input == nil, input != nil
	go func() {
		var settings api.UploadSettings
		var err error
		if input == nil {
			settings, err = client.GetUploadSettings(ctx)
		} else {
			settings, err = client.SetUploadSettings(ctx, *input)
		}
		cancelled := ctx.Err() != nil
		cancel()
		a.dispatch(func() {
			f := &a.uploadSettings
			if epoch != f.epoch || a.ctx.Err() != nil || cancelled || a.client != client || !a.connected || a.page != "Settings" {
				return
			}
			f.cancel = nil
			f.Loading, f.Saving = false, false
			if api.Unauthorized(err) {
				a.clearSession()
				a.fail(err)
				return
			}
			if input == nil {
				f.LoadError = err
			} else {
				f.SaveError = err
			}
			if err != nil {
				return
			}
			f.Confirmed, f.Loaded = settings, true
			f.resetDraft()
			if input != nil {
				f.Notice = "Upload settings saved"
			}
		})
	}()
}
func (a *uiApp) uploadSettingsView(c *ui.Context, p palette) {
	f := &a.uploadSettings
	disabled := a.busy || f.Loading || f.Saving || !f.Loaded
	a.settingsCard(c, p, "Uploads", func() {
		ui.Text(c, a.t("Large-file upload method")).FontSize(12).FontWeight(600).TextColor(p.muted)
		ui.Row(c).Gap(6).Wrap().Children(func() {
			for _, format := range []string{"bmp", "mp4"} {
				kind := "outline"
				if f.Format == format {
					kind = "tonal"
				}
				label := "MP4 parts"
				if format == "bmp" {
					label = "BMP parts"
				}
				a.button(c, "upload-format:"+format, label, nil, kind, disabled).AccessSelected(f.Format == format).OnClick(func() { f.Format = format; f.SaveError, f.Notice = nil, "" })
			}
		})
		ui.Row(c).Gap(12).Wrap().Children(func() {
			ui.Column(c).Grow(1).Basis(0).MinWidth(160).Children(func() {
				a.field(c, "upload-bmp-size", "BMP part size · 100–150 MB", &f.BMPMB, false).Disabled(disabled)
			})
			ui.Column(c).Grow(1).Basis(0).MinWidth(160).Children(func() {
				a.field(c, "upload-mp4-size", "MP4 part size · 1–9 GB", &f.MP4GB, false).Disabled(disabled)
			})
		})
		if f.Format == "mp4" {
			ui.Text(c, a.t("MP4 upstream does not support reliable resume")).FontSize(12).TextColor(p.muted)
		}
		if f.Loading || f.Saving {
			ui.Spinner(c).Label(a.t("Updating upload settings…"))
		}
		for _, err := range []error{f.LoadError, f.SaveError} {
			if err != nil {
				ui.Text(c, i18n.ErrorText(a.locale, err)).FontSize(12).TextColor(p.error).Selectable()
			}
		}
		if f.Notice != "" {
			ui.Text(c, a.t(f.Notice)).FontSize(12).TextColor(p.primary)
		}
		if f.LoadError != nil {
			a.button(c, "reload-upload-settings", "Reload upload settings", iRefresh, "outline", f.Loading || f.Saving || a.busy).AlignSelf(ui.Start).OnClick(a.loadUploadSettings)
		}
		a.button(c, "save-upload-settings", "Save upload settings", nil, "tonal", disabled).AlignSelf(ui.Start).OnClick(a.saveUploadSettings)
	})
}
