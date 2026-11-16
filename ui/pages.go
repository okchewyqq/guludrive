package workspace

import (
	"fmt"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
	"guludrive/ui/api"
	"guludrive/ui/i18n"
)

func (a *uiApp) connectionView(c *ui.Context, p palette) {
	ui.Scroll(c).Grow(1).MinHeight(0).FillWidth().Padding(24).Children(func() {
		ui.Row(c).FillWidth().Justify(ui.Center).Padding(24, 0).Children(func() {
			ui.Column(c).Width(432).MaxWidthPercent(100).Padding(24).Gap(12).Radius(cardRadius).Background(p.surface).Border(1, p.outline.Alpha(.6)).Children(func() {
				ui.Row(c).Gap(12).Children(func() {
					ui.Box(c).Size(36, 36).Radius(controlRadius).Background(p.primaryContainer).Center().Children(func() { ui.Icon(c, iBucket).FontSize(24).TextColor(p.onContainer) })
					ui.Text(c, "GuluDrive").FontSize(24).FontWeight(600)
				})
				title := "Sign in"
				if a.setupChecked && !a.initialized {
					title = "Create administrator"
				}
				ui.Text(c, a.t(title)).FontSize(20).FontWeight(600)
				a.banners(c, p)
				a.field(c, "server", "Server address", &a.server, false).Disabled(a.busy)
				ui.Text(c, a.t("Server address used by this window; saved with your appearance settings")).FontSize(12).TextColor(p.muted)
				a.field(c, "username", "Administrator username", &a.username, false).Disabled(a.busy || !a.setupChecked)
				input := a.field(c, fmt.Sprintf("password:%d", a.secretEpoch), "Administrator password", &a.password, true).Disabled(a.busy || !a.setupChecked)
				if input.Submitted() && !a.busy && a.setupChecked && a.initialized {
					a.connect()
				}
				label := "Sign in"
				if a.setupChecked && !a.initialized {
					label = "Create administrator"
					confirm := a.field(c, fmt.Sprintf("setup-confirm:%d", a.secretEpoch), "Confirm new password", &a.confirmPassword, true).Disabled(a.busy)
					if confirm.Submitted() && !a.busy {
						a.connect()
					}
				}
				if !a.setupChecked {
					label = "Check connection"
				}
				if a.busy {
					label = "Connecting…"
				}
				a.button(c, "connect", label, nil, "filled", a.busy).FillWidth().Margin(8, 0, 0, 0).OnClick(a.connect)
			})
		})
	})
}

func (a *uiApp) accountsView(c *ui.Context, p palette) {
	ui.Row(c).Gap(12).Wrap().Children(func() {
		pageHeading(c, a.t("Accounts"))
		ui.Spacer(c)
		a.button(c, "add-google", "Connect Google", iPlus, "filled", a.busy).OnClick(func() { a.openDialog("google", api.Entry{}) })
	})
	ui.Scroll(c).Grow(1).MinHeight(0).Gap(16).Children(func() {
		if len(a.snapshot.Accounts) == 0 {
			ui.Column(c).Padding(20).Radius(cardRadius).Border(1, p.outline).Gap(8).Children(func() {
				ui.Text(c, a.t("No connected Google accounts")).FontSize(20).FontWeight(600)
				ui.Text(c, a.t("Connect a Google account to create buckets and upload files.")).FontSize(13).TextColor(p.muted)
			})
		}
		for _, ac := range a.snapshot.Accounts {
			ui.Column(c.Key("account:"+ac.ID)).Padding(16).Gap(12).Radius(cardRadius).Background(p.surface).Border(1, p.outline.Alpha(.6)).Children(func() {
				ui.Row(c).Gap(14).Wrap().Children(func() {
					ui.Box(c).Size(40, 40).Radius(controlRadius).Background(p.surfaceHigh).Center().Children(func() { ui.Icon(c, iAccount).FontSize(24).TextColor(p.primary) })
					ui.Column(c).Grow(1).MinWidth(140).Gap(4).Children(func() {
						email := ac.Email
						if email == "" {
							email = a.t("Google account")
						}
						ui.Text(c, email).FontWeight(600).SingleLine().Tooltip(email)
						ui.Text(c, a.t("Account ID · %s", ac.ID)).FontSize(11).TextColor(p.muted).Selectable()
					})
					if ac.Selected {
						pill(c, a.t("Selected for new buckets"), p.primaryContainer, p.onContainer)
					} else {
						a.button(c, "select-account", "Use account", iCheck, "tonal", a.busy).OnClick(func() { a.selectAccount(ac.ID) })
					}
					a.iconButton(c, "remove-account", "Remove account", iDelete, a.busy).OnClick(func() { a.openDialog("remove-account", api.Entry{}); a.dialog.Account = ac })
				})
				count := 0
				for _, bucket := range a.snapshot.Buckets {
					if bucket.Album.AccountID == ac.ID {
						count++
					}
				}
				ui.Text(c, a.t("%d buckets", count)).FontSize(12).TextColor(p.muted)
			})
		}
	})
}

func (a *uiApp) copy(value, description string) {
	a.err, a.notice = "", ""
	a.currentError, a.noticeArgs = nil, nil
	// Unlike WriteText, Write reports clipboard failure. No build-lifetime
	// ui.Context is captured when a generated link arrives asynchronously.
	var err error
	if a.clipboardWrite != nil {
		err = a.clipboardWrite(value, description)
	} else {
		err = mygo.Clipboard.Write(transfer.TextData(value), mygo.ClipboardOptions{Concealed: description == "Download link" || description == "Worker script"})
	}
	if err != nil {
		a.fail(i18n.Err("Clipboard access failed. Try copying again."))
		return
	}
	a.notice = "%s copied"
	a.noticeArgs = []any{&i18n.Message{ID: description}}
}

func (a *uiApp) settingsView(c *ui.Context, p palette) {
	pageHeading(c, a.t("Settings"))
	ui.Scroll(c).Grow(1).MinHeight(0).Gap(16).Children(func() {
		a.settingsCard(c, p, "Connection", func() {
			a.copyField(c, "server", "Server address", a.client.BaseURL())
			a.button(c, "switch-server", "Switch server", nil, "text", a.busy).AlignSelf(ui.Start).OnClick(a.switchServer)
			a.copyField(c, "webdav", "WebDAV address", a.client.WebDAVURL())
			a.copyField(c, "username", "Username", a.snapshot.Admin.Username)
			a.button(c, "sign-out", "Sign out", iLock, "outline", a.busy).AlignSelf(ui.Start).OnClick(a.logout)
		})
		a.adminSettingsView(c, p)
		a.uploadSettingsView(c, p)
		a.downloadSettingsView(c, p)
		a.settingsCard(c, p, "Appearance", func() {
			ui.Column(c).Gap(14).Children(func() {
				ui.Row(c).Gap(12).Wrap().Padding(2, 0).Children(func() {
					ui.Text(c, a.t("Language")).Grow(1).FontWeight(600)
					a.languagePicker(c, "settings-language")
				})
				ui.Row(c).Gap(12).Wrap().Padding(2, 0).Children(func() {
					ui.Text(c, a.t("Theme")).Grow(1).FontWeight(600)
					a.themePicker(c)
				})
			})
		})
	})
}

func (a *uiApp) settingsCard(c *ui.Context, p palette, title string, children func()) {
	ui.Column(c.Key("settings:"+title)).Padding(16).Gap(10).Radius(cardRadius).Background(p.surface).Border(1, p.outline.Alpha(.6)).Children(func() {
		ui.Text(c, a.t(title)).FontSize(19).FontWeight(600)
		children()
	})
}
