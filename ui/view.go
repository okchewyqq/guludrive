package workspace

import (
	"fmt"
	"path"
	"strings"

	"github.com/egoist/mygo/ui"
	"guludrive/ui/api"
	"guludrive/ui/preferences"
	"guludrive/ui/state"
)

func (a *uiApp) view(c *ui.Context) {
	// Each frame starts with MyGo's current system appearance. Never freeze it.
	a.dark = c.Theme().Dark
	if a.choices.Theme != preferences.System {
		a.dark = a.choices.Theme == preferences.Dark
	}
	p := applyTheme(c, a.dark)
	width, _ := c.Size()
	ui.Column(c.Key("workspace")).Fill().Children(func() {
		a.titleBar(c, p)
		a.topBar(c, p, width)
		if !a.connected {
			a.connectionView(c, p)
			return
		}
		ui.Row(c).Grow(1).MinHeight(0).FillWidth().AlignItems(ui.Stretch).Children(func() {
			if width >= 680 {
				a.sidebar(c, p, true)
			}
			ui.Column(c.Key("content")).Grow(1).Basis(0).MinWidth(0).FillHeight().Padding(12, 20, 16, 20).Gap(12).Children(func() {
				a.banners(c, p)
				ui.Box(c.Key("page-stage")).Grow(1).MinHeight(0).FillWidth().Clip().Children(func() {
					key := a.page
					if key == "Files" {
						key += ":" + a.path
					}
					page := ui.Column(c.Key("page:" + key)).Absolute().Fill().Gap(12).Transition(pageTransition)
					enterPose(c, page, ui.Pose{Y: 6, Fade: 1}, pageAnimation)
					page.Children(func() {
						switch a.page {
						case "Accounts":
							a.accountsView(c, p)
						case "Settings":
							a.settingsView(c, p)
						default:
							a.filesView(c, p, width)
						}
					})
				})
			})
		})
		if width < 680 {
			a.mobileNav(c, p)
		}
	})
	a.dialogView(c, p)
}
func (a *uiApp) setPage(page string) {
	if page != a.page {
		a.cancelAdminEdit()
		if a.page == "Settings" {
			a.stopProxyWork()
			if a.downloadApplying != "" {
				a.operationEpoch++
				if a.activeCancel != nil {
					a.activeCancel()
				}
				a.activeCancel = nil
				a.busy = false
				a.downloadApplying = ""
			}
			a.resetDownloadDraft()
			a.stopUploadSettings()
		}
	}
	changed := page != a.page
	a.page = page
	if changed && page == "Settings" {
		a.loadUploadSettings()
	}
}

// titleBar draws the title bar the page owns (mygo.TitleBarHidden with
// HideWindowButtons): it drags the window, shows the brand and the window
// controls, and takes no room in full screen, where the window has none.
// Context.TitleBar is zero here because the system controls are hidden, so
// the bar keeps its own height and clears any room the platform does report.
func (a *uiApp) titleBar(c *ui.Context, p palette) {
	if a.window != nil && a.window.IsFullScreen() {
		return
	}
	bar := c.TitleBar()
	row := ui.Row(c.Key("title-bar")).FillWidth().Height(max(44, bar.Height)).Shrink(0).
		Padding(0, 8+bar.Right, 0, 8+bar.Left).Gap(8).DragWindow()
	row.Children(func() {
		ui.Box(c).Size(20, 20).Radius(controlRadius).Background(p.primaryContainer).Center().Shrink(0).Children(func() {
			ui.Icon(c, iBucket).FontSize(14).TextColor(p.onContainer)
		})
		ui.Text(c, "GuluDrive").FontSize(13).FontWeight(600).SingleLine()
		ui.Spacer(c)
		a.windowControl(c, p, "window-minimize", "Minimize", iMinimize, a.window.Minimize)
		maximize, label, icon := "window-maximize", "Maximize", iMaximize
		if a.window.IsMaximized() {
			label, icon = "Restore", iRestore
		}
		a.windowControl(c, p, maximize, label, icon, a.window.ToggleMaximize).MaximizeButton()
		a.windowControl(c, p, "window-close", "Close window", iClose, a.window.Close)
	})
}

// windowControl draws one of the window controls the view owns: a flat hit
// area that highlights on hover, red for close, as the system's do.
func (a *uiApp) windowControl(c *ui.Context, p palette, key, label string, icon *ui.SVG, action func()) ui.Element {
	b := ui.ButtonBase(c.Key(key)).Label(a.t(label)).Tooltip(a.t(label)).Size(46, 32).Center().
		Radius(0).Background(ui.Transparent).TextColor(p.onSurface).Cursor(ui.CursorPointer)
	if b.Hovered() || b.Pressed() {
		bg, fg := p.variant, p.onSurface
		if key == "window-close" {
			bg, fg = p.error, p.onPrimary
		}
		b.Background(bg).TextColor(fg)
	}
	b.OnClick(action)
	return animateControl(b, false).Children(func() { ui.Icon(c, icon).FontSize(14) })
}

// topBar carries the transient status that the title bar has no room for: the
// GDirect badge and the busy label. It takes no room when both are empty.
func (a *uiApp) topBar(c *ui.Context, p palette, width float32) {
	if !a.topBarVisible() {
		return
	}
	padding, gap := float32(20), float32(12)
	if width < 680 {
		padding, gap = 12, 4
	}
	ui.Row(c).FillWidth().Height(64).Padding(12, padding).Gap(gap).Children(func() {
		ui.Spacer(c)
		a.gdirectBadge(c, p, width)
		if a.busy {
			ui.Spinner(c).Label(a.busyText()).TextColor(p.primary)
			if width > 760 {
				ui.Text(c, a.busyText()).MaxWidth(260).SingleLine().FontSize(12).TextColor(p.muted)
			}
		}
	})
}

func (a *uiApp) topBarVisible() bool {
	return a.busy || (a.connected && a.snapshot.Status.GDirect.Enabled)
}
func (a *uiApp) sidebar(c *ui.Context, p palette, expanded bool) {
	w := float32(76)
	pad := float32(8)
	if expanded {
		w = 240
	}
	ui.Column(c).Width(w).Shrink(0).FillHeight().Padding(pad).Gap(4).Radius(0).Background(p.surfaceLow).Children(func() {
		for _, item := range []struct {
			name string
			icon *ui.SVG
		}{{"Files", iFolder}, {"Accounts", iAccount}, {"Settings", iSettings}} {
			a.navItem(c, p, item.name, item.icon, expanded)
		}
		if !expanded {
			ui.Spacer(c)
			return
		}
		ui.Divider(c).Background(p.outline.Alpha(.6)).Margin(8, 0)
		ui.Row(c).Padding(0, 4).Children(func() {
			ui.Text(c, a.t("BUCKETS / ALBUMS")).FontSize(10).LetterSpacing(1.2).FontWeight(700).TextColor(p.muted).Grow(1)
			a.iconButton(c, "side-add-bucket", "Create bucket", iPlus, a.busy || a.googleSetupNeeded()).Size(32, 32).OnClick(func() { a.openDialog("bucket", api.Entry{}) })
		})
		ui.Scroll(c).Grow(1).MinHeight(64).Gap(4).Children(func() {
			if len(a.snapshot.Buckets) == 0 {
				ui.Text(c, a.t("No buckets")).FontSize(12).TextColor(p.muted).Padding(8, 4)
			}
			for _, b := range a.snapshot.Buckets {
				dest := "/" + b.Name
				active := a.path == dest || strings.HasPrefix(a.path, dest+"/")
				row := ui.ButtonBase(c.Key(fmt.Sprintf("bucket:%d", b.ID))).FillWidth().Height(54).Padding(0, 8).Gap(8).Justify(ui.Start).Radius(controlRadius).TextColor(p.muted).Background(ui.Transparent).Disabled(a.busy).AccessSelected(active).Cursor(ui.CursorPointer)
				if active {
					row.Background(p.primaryContainer).TextColor(p.onContainer)
				} else if row.Hovered() {
					row.Background(p.variant)
				}
				animateControl(row, a.busy).Children(func() {
					selectionAccent(c, p, active)
					ui.Icon(c, iBucket).FontSize(18)
					ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
						ui.Text(c, b.Name).FontSize(13).FontWeight(600).SingleLine().Tooltip(b.Name)
						email := a.bucketEmail(b)
						ui.Text(c, email).FontSize(11).TextColor(p.muted).SingleLine().Tooltip(email)
					})
				})
				row.OnClick(func() { a.browse(dest, true) })
			}
		})
	})
}
func (a *uiApp) navItem(c *ui.Context, p palette, name string, icon *ui.SVG, expanded bool) {
	b := ui.ButtonBase(c.Key("nav:"+name)).Label(a.t(name)).FillWidth().Height(40).Radius(controlRadius).Padding(0, 8).Gap(10).Justify(ui.Start).Cursor(ui.CursorPointer).Background(ui.Transparent).AccessSelected(a.page == name)
	if a.page == name {
		b.Background(p.primaryContainer).TextColor(p.onContainer)
	} else {
		b.TextColor(p.muted)
		if b.Hovered() {
			b.Background(p.variant)
		}
	}
	if !expanded {
		b.Center().Padding(0).Tooltip(a.t(name))
	}
	animateControl(b, false).Children(func() {
		if expanded {
			selectionAccent(c, p, a.page == name)
		}
		ui.Icon(c, icon).FontSize(21)
		if expanded {
			ui.Text(c, a.t(name)).FontWeight(600)
		}
	})
	b.OnClick(func() { a.setPage(name) })
}
func (a *uiApp) mobileNav(c *ui.Context, p palette) {
	ui.Row(c).FillWidth().Height(56).Padding(4, 8).Gap(4).Background(p.surfaceLow).Children(func() {
		for _, item := range []struct {
			name string
			icon *ui.SVG
		}{{"Files", iFolder}, {"Accounts", iAccount}, {"Settings", iSettings}} {
			b := ui.ButtonBase(c.Key("mobile:" + item.name)).Grow(1).Height(48).Radius(controlRadius).Center().Gap(4).Column().Label(a.t(item.name)).Background(ui.Transparent).AccessSelected(a.page == item.name)
			if a.page == item.name {
				b.Background(p.primaryContainer).TextColor(p.onContainer)
			} else {
				b.TextColor(p.muted)
				if b.Hovered() {
					b.Background(p.variant)
				}
			}
			animateControl(b, false).Children(func() { ui.Icon(c, item.icon).FontSize(20); ui.Text(c, a.t(item.name)).FontSize(10) })
			b.OnClick(func() { a.setPage(item.name) })
		}
	})
}
func (a *uiApp) banners(c *ui.Context, p palette) {
	if a.err != "" && !a.dialog.Open {
		ui.Row(c).FillWidth().Padding(12, 16).Gap(12).Radius(cardRadius).Background(p.errorContainer).Children(func() {
			ui.Icon(c, iInfo).FontSize(20).TextColor(p.error)
			ui.Text(c, a.errorText()).FontSize(13).TextColor(p.error).Grow(1).MinWidth(0).Selectable()
			a.iconButton(c, "dismiss-error", "Dismiss error", iClose, false).OnClick(func() { a.err = "" })
		})
	}
	if a.notice != "" {
		ui.Row(c).FillWidth().Padding(8, 16).Gap(12).Radius(cardRadius).Background(p.primaryContainer).Children(func() {
			ui.Icon(c, iCheck).FontSize(20).TextColor(p.onContainer)
			ui.Text(c, a.noticeText()).FontSize(13).TextColor(p.onContainer).Grow(1).MinWidth(0)
			a.iconButton(c, "dismiss-notice", "Dismiss message", iClose, false).OnClick(func() { a.notice = "" })
		})
	}
}
func pageHeading(c *ui.Context, title string) {
	ui.Text(c, title).FontSize(30).FontWeight(600).LetterSpacing(-.8).Shrink(0)
}
func (a *uiApp) filesView(c *ui.Context, p palette, width float32) {
	title := a.t("Files")
	if a.path != "/" {
		title = path.Base(a.path)
	}
	ui.Row(c).Gap(12).Wrap().Children(func() {
		ui.Column(c).Grow(1).MinWidth(180).Children(func() { pageHeading(c, title) })
		ui.Row(c).Gap(8).Wrap().Children(func() {
			if a.path == "/" {
				a.button(c, "new-bucket", "New bucket", iPlus, "tonal", a.busy || a.googleSetupNeeded()).OnClick(func() { a.openDialog("bucket", api.Entry{}) })
			} else {
				a.button(c, "new-directory", "New folder", iPlus, "outline", a.busy).OnClick(func() { a.openDialog("directory", api.Entry{}) })
			}
			a.button(c, "upload", "Upload files", iUpload, "filled", a.busy || a.path == "/").Tooltip(a.t("Choose a bucket or folder to upload into")).OnClick(a.pickUploads)
		})
	})
	ui.ScrollHorizontal(c).Height(40).Shrink(0).Children(func() {
		ui.Row(c).Gap(2).Children(func() {
			for i, crumb := range state.Crumbs(a.path) {
				if i > 0 {
					ui.Icon(c, iChevron).FontSize(14).TextColor(p.muted)
				}
				name := crumb.Name
				if crumb.Path == "/" {
					name = a.t("All buckets")
				}
				button := mdButton(c, "crumb:"+crumb.Path, name, nil, "text", a.busy).Height(36).Padding(0, 10).MaxWidth(240)
				button.OnClick(func() { a.browse(crumb.Path, true) })
			}
		})
	})
	ui.Row(c).Gap(12).Children(func() {
		ui.Row(c).Height(44).Grow(1).Basis(0).MinWidth(100).Gap(8).Padding(0, 12).Radius(controlRadius).Background(p.surfaceLow).Children(func() {
			ui.Icon(c, iSearch).FontSize(20).TextColor(p.muted)
			ui.TextInputBase(c.Key("file-search"), &a.query).Placeholder(a.t("Find in this folder")).Label(a.t("Find files")).Grow(1).MinWidth(0).Height(40).FontSize(14)
			if a.query != "" {
				a.iconButton(c, "clear-search", "Clear search", iClose, false).Size(32, 32).OnClick(func() { a.query = "" })
			}
		})
		a.sortPicker(c)
	})
	items := state.Entries(a.snapshot.Listing.Entries, a.query, a.order)
	ui.Column(c).Grow(1).MinHeight(80).Radius(cardRadius).Background(p.surface).Border(1, p.outline.Alpha(.6)).Clip().Padding(8).Children(func() {
		if len(items) == 0 {
			a.emptyFiles(c, p)
			return
		}
		if width >= 1040 {
			ui.Row(c).Height(36).Padding(0, 20).Gap(16).TextColor(p.muted).FontSize(11).FontWeight(600).Children(func() {
				ui.Text(c, a.t("NAME")).Grow(1).MinWidth(0).Margin(0, 0, 0, 40)
				ui.Text(c, a.t("SIZE")).Width(86)
				ui.Text(c, a.t("MODIFIED")).Width(158)
				ui.Text(c, a.t("TYPE")).Width(120)
				ui.Box(c).Width(20)
			})
		}
		a.list.Key = func(i int) any { return items[i].Path }
		ui.List(c.Key("file-list:"+a.path), &a.list, len(items), func(i int) { a.fileRow(c, p, items[i], width >= 1040) }).Grow(1).MinHeight(0)
	})
	selected := a.selectedEntries()
	if len(selected) > 1 {
		a.batchSelectionActions(c, p, selected)
	} else if e, ok := a.selection(); ok {
		a.selectionActions(c, p, e)
	} else {
		ui.Row(c).Padding(0, 4).Children(func() {
			ui.Text(c, a.t("%d items", len(items))).FontSize(11).TextColor(p.muted)
			ui.Spacer(c)
			ui.Text(c, a.t("Click to select multiple items · Double-click folders to open")).FontSize(11).TextColor(p.muted).SingleLine().MaxWidth(390)
		})
	}
}
func (a *uiApp) fileRow(c *ui.Context, p palette, e api.Entry, wide bool) {
	selected := a.selected[e.Path]
	row := ui.ButtonBase(c.Key(e.Path)).FillWidth().Height(60).Radius(controlRadius).Gap(12).Padding(8, 12).Cursor(ui.CursorPointer).Disabled(a.busy).FocusRingOffset(-3).Background(ui.Transparent).Transition(controlTransition)
	if selected {
		row.Background(p.primaryContainer).TextColor(p.onContainer).AccessSelected(true)
	} else if row.Hovered() {
		row.Background(p.surfaceLow)
	}
	row.Children(func() {
		icon := iFile
		if e.IsDir {
			icon = iFolder
			if strings.Count(strings.Trim(e.Path, "/"), "/") == 0 {
				icon = iBucket
			}
		}
		bg := p.surfaceHigh
		if selected {
			bg = p.onContainer.Alpha(.1)
		}
		ui.Box(c).Size(36, 36).Radius(controlRadius).Background(bg).Center().Shrink(0).Children(func() { ui.Icon(c, icon).FontSize(21).TextColor(p.primary) })
		ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
			ui.Text(c, e.Name).FontSize(14).FontWeight(550).SingleLine().Tooltip(e.Name)
			if !wide {
				subtitle := a.kind(e)
				if !e.IsDir {
					subtitle += " · " + a.bytes(e.Size)
				}
				ui.Text(c, subtitle+" · "+a.date(e.ModTime)).FontSize(11).TextColor(p.muted).SingleLine()
			}
		})
		if wide {
			size := "—"
			if !e.IsDir {
				size = a.bytes(e.Size)
			}
			ui.Text(c, size).Width(86).FontSize(12).TextColor(p.muted).SingleLine()
			ui.Text(c, a.date(e.ModTime)).Width(158).FontSize(11).TextColor(p.muted).SingleLine()
			ui.Text(c, a.kind(e)).Width(120).FontSize(11).TextColor(p.muted).SingleLine().Tooltip(a.kind(e))
		}
		if e.IsDir {
			ui.Icon(c, iChevron).FontSize(18).TextColor(p.muted)
		} else {
			ui.Box(c).Width(18)
		}
	})
	if row.DoubleClicked() && e.IsDir && !a.busy {
		a.browse(e.Path, true)
	}
	row.OnClick(func() { a.toggleSelection(e.Path) })
}
func (a *uiApp) googleSetupNeeded() bool {
	return !a.snapshot.Status.GoogleReady
}
func (a *uiApp) emptyFiles(c *ui.Context, p palette) {
	ui.Column(c).Grow(1).FillWidth().Center().Padding(24).Gap(12).Children(func() {
		if a.busy {
			ui.Spinner(c).Size(36, 36).Label(a.t("Loading files"))
			ui.Text(c, a.t("Loading files")).TextColor(p.muted)
			return
		}
		ui.Box(c).Size(48, 48).Radius(cardRadius).Background(p.primaryContainer).Center().Children(func() { ui.Icon(c, iFolder).FontSize(28).TextColor(p.onContainer) })
		title, body := "No files", "Upload files or create a folder."
		if a.path == "/" {
			title, body = "No buckets", "Create a bucket to upload files."
			if a.googleSetupNeeded() {
				body = "Connect a Google account to create buckets and upload files."
			}
		}
		if a.query != "" {
			title, body = "No matching files", "Try another filename."
		}
		ui.Text(c, a.t(title)).FontSize(22).FontWeight(600).TextAlign(ui.Center)
		ui.Text(c, a.t(body)).FontSize(13).TextColor(p.muted).MaxWidth(420).TextAlign(ui.Center)
		if a.query != "" {
			a.button(c, "empty-clear", "Clear search", nil, "tonal", false).OnClick(func() { a.query = "" })
		} else if a.googleSetupNeeded() {
			a.button(c, "empty-google", "Connect Google", iCloud, "filled", a.busy).OnClick(func() { a.openDialog("google", api.Entry{}) })
		} else if a.path == "/" {
			a.button(c, "empty-bucket", "Create bucket", iPlus, "filled", a.busy).OnClick(func() { a.openDialog("bucket", api.Entry{}) })
		} else {
			a.button(c, "empty-upload", "Choose files", iUpload, "filled", a.busy).OnClick(a.pickUploads)
		}
	})
}
func (a *uiApp) batchSelectionActions(c *ui.Context, p palette, selected []api.Entry) {
	files := 0
	for _, e := range selected {
		if !e.IsDir {
			files++
		}
	}
	ui.Column(c).Gap(8).Padding(12, 16).Radius(cardRadius).Background(p.surfaceLow).Children(func() {
		ui.Row(c).Gap(10).Children(func() {
			ui.Text(c, a.t("%d selected", len(selected))).FontWeight(600).Grow(1).MinWidth(0)
			a.iconButton(c, "clear-selection", "Clear selection", iClose, false).Size(32, 32).OnClick(a.clearSelection)
		})
		ui.Row(c).Gap(6).Wrap().Children(func() {
			label := "Copy download links"
			if files == 1 {
				label = "Copy download link"
			}
			a.button(c, "download-selection", label, iCopy, "tonal", a.busy || files == 0).OnClick(a.copyDownloadLinks)
			for _, item := range []struct {
				kind, label string
				icon        *ui.SVG
			}{{"move", "Move", iMove}, {"copy", "Copy", iCopy}, {"delete", "Delete", iDelete}} {
				a.button(c, "batch:"+item.kind, item.label, item.icon, "text", a.busy).Height(40).Padding(0, 12).OnClick(func() { a.openBatchDialog(item.kind, selected) })
			}
			if files != len(selected) {
				ui.Text(c, a.t("Folders are excluded from download links")).FontSize(11).TextColor(p.muted).Padding(10, 4)
			}
		})
	})
}
func (a *uiApp) selectionActions(c *ui.Context, p palette, e api.Entry) {
	ui.Column(c).Gap(8).Padding(12, 16).Radius(cardRadius).Background(p.surfaceLow).Children(func() {
		ui.Row(c).Gap(10).Children(func() {
			ui.Text(c, e.Name).FontWeight(600).SingleLine().Grow(1).MinWidth(0)
			pill(c, a.kind(e), p.variant, p.muted)
			a.iconButton(c, "clear-selection", "Clear selection", iClose, false).Size(32, 32).OnClick(a.clearSelection)
		})
		ui.Row(c).Gap(6).Wrap().Children(func() {
			if e.IsDir {
				a.button(c, "open-selection", "Open", iFolder, "tonal", a.busy).OnClick(func() { a.browse(e.Path, true) })
			} else {
				a.button(c, "download-selection", "Copy download link", iCopy, "tonal", a.busy).OnClick(func() { a.copyDownloadLink(e) })
			}
			for _, item := range []struct {
				kind, label string
				icon        *ui.SVG
			}{{"rename", "Rename", iEdit}, {"move", "Move", iMove}, {"copy", "Copy", iCopy}, {"delete", "Delete", iDelete}} {
				a.button(c, "selection:"+item.kind, item.label, item.icon, "text", a.busy).Height(40).Padding(0, 12).OnClick(func() { a.openDialog(item.kind, e) })
			}
		})
	})
}
