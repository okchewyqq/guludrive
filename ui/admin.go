package workspace

import (
	"context"
	"fmt"

	"guludrive/ui/api"
	"guludrive/ui/i18n"

	"github.com/egoist/mygo/ui"
)

type adminForm struct {
	Editing                                                 bool
	Username, CurrentPassword, NewPassword, ConfirmPassword string
	Epoch                                                   int
}

func (a *uiApp) clearAdminSecrets() {
	a.admin.CurrentPassword, a.admin.NewPassword, a.admin.ConfirmPassword = "", "", ""
	a.admin.Epoch++ // Drop the password editors and their undo histories.
}

func (a *uiApp) cancelAdminEdit() {
	a.clearAdminSecrets()
	a.admin.Editing = false
	a.admin.Username = a.snapshot.Admin.Username
}

func (a *uiApp) editAdmin() {
	if a.busy || !a.connected {
		return
	}
	a.cancelAdminEdit()
	a.admin.Editing = true
	a.err, a.notice = "", ""
	a.currentError, a.noticeArgs = nil, nil
}

func (a *uiApp) saveAdmin() {
	if a.busy || !a.connected || a.client == nil || !a.admin.Editing {
		return
	}
	input := api.AdminUpdate{CurrentPassword: a.admin.CurrentPassword, NewPassword: a.admin.NewPassword}
	if a.admin.Username != a.snapshot.Admin.Username {
		username := a.admin.Username
		input.Username = &username
	}
	confirm := a.admin.ConfirmPassword
	a.clearAdminSecrets()
	if input.NewPassword != confirm {
		a.fail(i18n.Err("The new password and confirmation do not match"))
		return
	}
	c := a.client
	a.launch("Update administrator credentials", func(ctx context.Context) (func(), error) {
		profile, err := c.UpdateAdmin(ctx, input)
		if err != nil {
			return nil, err
		}
		return func() {
			a.clearSession() // No DELETE /api/session with an already-invalid cookie.
			a.username = profile.Username
			a.err, a.currentError = "", nil
			a.notice = "Administrator credentials updated. Sign in again."
			a.noticeArgs = nil
		}, nil
	})
}

func (a *uiApp) adminSettingsView(c *ui.Context, p palette) {
	a.settingsCard(c, p, "Administrator account", func() {
		ui.Text(c, a.t("Current username · %s", a.snapshot.Admin.Username)).FontSize(13).TextColor(p.muted).Selectable()
		if !a.admin.Editing {
			a.button(c, "edit-admin", "Change username or password", iEdit, "tonal", a.busy).AlignSelf(ui.Start).OnClick(a.editAdmin)
			return
		}
		a.field(c, "admin-username", "Administrator username", &a.admin.Username, false).Disabled(a.busy)
		a.field(c, fmt.Sprintf("admin-current:%d", a.admin.Epoch), "Current password", &a.admin.CurrentPassword, true).Disabled(a.busy)
		a.field(c, fmt.Sprintf("admin-new:%d", a.admin.Epoch), "New password", &a.admin.NewPassword, true).Disabled(a.busy)
		a.field(c, fmt.Sprintf("admin-confirm:%d", a.admin.Epoch), "Confirm new password", &a.admin.ConfirmPassword, true).Disabled(a.busy)
		ui.Row(c).Gap(8).Wrap().Children(func() {
			a.button(c, "admin-cancel", "Cancel", nil, "text", a.busy).OnClick(a.cancelAdminEdit)
			a.button(c, "admin-save", "Update administrator credentials", iLock, "filled", a.busy).OnClick(a.saveAdmin)
		})
	})
}
