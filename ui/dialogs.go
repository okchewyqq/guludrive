package workspace

import (
	"fmt"

	"github.com/egoist/mygo/ui"
)

const googleSetupURL = "https://accounts.google.com/EmbeddedSetup"

func (a *uiApp) dialogView(c *ui.Context, p palette) {
	width, height := c.Size()
	// Keep dimming separate from the transparent modal shell: its exit scales
	// only the visible panel, never the window-sized dimming surface. MyGo
	// retains exiting snapshots without input or focus; no closing timer holds
	// an invisible modal over the page.
	ui.Overlay(c.Key("dialog-shade-layer"), func() {
		if a.dialog.Open {
			shade := ui.Box(c.Key("dialog-shade")).Absolute().Fill().PassThrough().
				Background(ui.RGBA(0, 20, 10, .44)).Transition(shadeTransition)
			enterPose(c, shade, ui.Pose{Fade: 1}, dialogAnimation)
		}
	})
	panel := ui.DialogBase(c.Key("workspace-dialog"), &a.dialog.Open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.Transparent).Transition(dialogTransition)
		enterPose(c, backdrop, ui.Pose{Y: 8, Scale: .985, Fade: 1}, dialogAnimation)
		panel.Width(520).MaxWidth(width-32).MaxHeight(height-32).Padding(24).Gap(12).Radius(dialogRadius).Background(p.surface).
			Shadow(0, 16, 48, 0, ui.RGBA(0, 0, 0, .18))
		d := &a.dialog
		title := map[string]string{"bucket": "Create a bucket", "directory": "New folder", "rename": "Rename", "move": "Move to…", "copy": "Make a copy", "delete": "Delete permanently?", "google": "Connect Google", "remove-account": "Remove this account?", "upload": "Upload files"}[d.Kind]
		ui.Row(c).Gap(12).Children(func() {
			ui.Text(c, a.t(title)).FontSize(25).FontWeight(600).LetterSpacing(-.5).Grow(1).MinWidth(0)
			a.iconButton(c, "close-dialog", "Close dialog", iClose, a.busy).OnClick(a.closeDialog)
		})
		// The body scrolls on small windows; actions remain visible.
		ui.Scroll(c).MaxHeight(height - 220).MinHeight(28).Gap(12).Children(func() {
			switch d.Kind {
			case "bucket":
				ui.Text(c, a.t("The album is created with the first upload.")).FontSize(13).TextColor(p.muted)
				a.field(c, "bucket-name", "Bucket name", &d.Value, false).AutoFocus().Disabled(a.busy)
			case "directory":
				ui.Text(c, a.t("Create inside %s", a.path)).FontSize(12).TextColor(p.muted).Selectable()
				a.field(c, "directory-name", "Folder name", &d.Value, false).AutoFocus().Disabled(a.busy)
			case "rename":
				ui.Text(c, a.t("Original name · %s", d.Target.Name)).FontSize(12).TextColor(p.muted).Selectable()
				a.field(c, "new-name", "New name", &d.Value, false).AutoFocus().Disabled(a.busy)
			case "move", "copy":
				batch := len(d.Targets) > 1
				if batch {
					ui.Text(c, a.t("%d selected", len(d.Targets))).FontSize(12).TextColor(p.muted)
				} else {
					ui.Text(c, a.t("Source · %s", d.Target.Path)).FontSize(12).TextColor(p.muted).Selectable()
				}
				if batch {
					a.field(c, "destination", "Destination folder", &d.Value, false).AutoFocus().Disabled(a.busy)
				} else {
					a.field(c, "destination", "Full destination path (including name)", &d.Value, false).AutoFocus().Disabled(a.busy)
					ui.Text(c, a.t("Use a full path such as /Documents/Archive/report.pdf. Relative paths start in the current folder. The destination parent must exist.")).FontSize(11).TextColor(p.muted)
				}
				ui.Checkbox(c, &d.Overwrite, a.t("Allow replacement of existing destination")).Disabled(a.busy)
				if d.Target.IsDir {
					ui.Checkbox(c, &d.Recursive, a.t("Include the folder's contents (recursive)")).Disabled(a.busy)
				}
				if batch {
					ui.List(c.Key("batch-transfer-preview"), nil, len(d.Targets), func(i int) {
						e := d.Targets[i]
						ui.Row(c).Padding(8, 0).Gap(12).Children(func() {
							ui.Icon(c, iFile).FontSize(18).TextColor(p.primary)
							ui.Text(c, e.Name).Grow(1).MinWidth(0).SingleLine().Tooltip(e.Name)
						})
					}).Height(min(float32(len(d.Targets))*38, 152))
				}
			case "delete":
				ui.Icon(c, iDelete).FontSize(32).TextColor(p.error)
				if len(d.Targets) > 1 {
					ui.Text(c, a.t("%d selected", len(d.Targets))).FontWeight(600)
					ui.List(c.Key("batch-delete-preview"), nil, len(d.Targets), func(i int) {
						ui.Text(c, d.Targets[i].Path).Padding(8, 0).SingleLine().Tooltip(d.Targets[i].Path).Selectable()
					}).Height(min(float32(len(d.Targets))*38, 152))
				} else {
					ui.Text(c, d.Target.Path).FontWeight(600).Selectable()
				}
				ui.Text(c, a.t("This permanently removes the entry. There is no trash. A non-empty folder must be emptied first.")).FontSize(13).TextColor(p.muted)
			case "google":
				ui.Text(c, a.t("1. Open the link and sign in to Google.")).FontSize(13)
				a.copyField(c, "google-setup", "Google sign-in link", googleSetupURL)
				ui.Link(c, a.t("Open in browser ↗"), googleSetupURL).TextColor(p.primary).FontWeight(600)
				if a.notice != "" {
					ui.Text(c, a.noticeText()).FontSize(12).TextColor(p.primary)
				}
				ui.Text(c, a.t("2. In developer tools → Application/Storage → Cookies, copy only the oauth_token value and paste it below.")).FontSize(13)
				ui.Text(c, a.t("Google's consent page may stay open. Once its cookies contain oauth_token, return here; no automatic redirect is needed.")).FontSize(12).TextColor(p.muted)
				a.field(c, fmt.Sprintf("oauth:%d", a.secretEpoch), "oauth_token (secret)", &a.oauth, true).Disabled(a.busy)
				ui.Text(c, a.t("Sent to your GuluDrive server; kept in memory only and cleared on submit or cancel. Use a trusted server.")).FontSize(11).TextColor(p.muted)
			case "remove-account":
				ui.Text(c, d.Account.Email).FontSize(18).FontWeight(600)
				ui.Text(c, a.t("This removes stored credentials from the server. Buckets may still be bound to this account and could become unavailable. The server may reject removal while it is in use.")).FontSize(13).TextColor(p.muted)
			case "upload":
				a.field(c, "upload-directory", "Destination folder", &d.Value, false).Disabled(a.busy)
				ui.Text(c, a.t("Files are not uploaded until you press Upload. Keep this app open until the transfer completes.")).FontSize(12).TextColor(p.muted)
				var total int64
				for _, f := range a.uploads {
					total += f.Size
				}
				ui.Text(c, a.t("%d files · %s", len(a.uploads), a.bytes(total))).FontSize(13).FontWeight(600)
				ui.List(c.Key("upload-preview"), nil, len(a.uploads), func(i int) {
					f := a.uploads[i]
					ui.Row(c).Padding(8, 0).Gap(12).Children(func() {
						ui.Icon(c, iFile).FontSize(18).TextColor(p.primary)
						ui.Text(c, f.Name).Grow(1).MinWidth(0).SingleLine().Tooltip(f.Name)
						ui.Text(c, a.bytes(f.Size)).FontSize(11).TextColor(p.muted)
					})
				}).Height(min(float32(len(a.uploads))*38, 152))
				ui.Checkbox(c, &d.Overwrite, a.t("Replace existing files")).Disabled(a.busy)
			}
			if a.err != "" {
				ui.Text(c, a.errorText()).FontSize(12).TextColor(p.error).Selectable().Padding(12).Radius(controlRadius).Background(p.errorContainer)
			}
			if a.busy {
				ui.Row(c).Gap(10).Children(func() {
					ui.Spinner(c).Label(a.busyText())
					ui.Text(c, a.busyText()).FontSize(12).TextColor(p.muted).Grow(1).MinWidth(0)
				})
			}
		})
		ui.Row(c).Gap(8).Justify(ui.End).Wrap().Children(func() {
			a.button(c, "dialog-cancel", "Cancel", nil, "text", a.busy).OnClick(a.closeDialog)
			if a.busy && a.activeCancel != nil {
				a.button(c, "dialog-stop", "Stop request", iClose, "outline", false).OnClick(a.activeCancel)
			}
			label := map[string]string{"bucket": "Create bucket", "directory": "Create folder", "rename": "Rename", "move": "Move", "copy": "Copy", "delete": "Delete permanently", "google": "Exchange & connect", "remove-account": "Remove account", "upload": "Upload files"}[d.Kind]
			kind := "filled"
			if d.Kind == "delete" || d.Kind == "remove-account" {
				kind = "danger"
			}
			a.button(c, "dialog-submit", label, nil, kind, a.busy).Label(a.t("Confirm %s", a.t(label))).OnClick(a.submitDialog)
		})
	})
	if panel.Valid() && panel.Dismissed() {
		if a.busy {
			a.dialog.Open = true
		} else {
			a.closeDialog()
		}
	}
}
