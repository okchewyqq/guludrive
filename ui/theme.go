package workspace

import (
	"github.com/egoist/mygo/ui"
)

// MD3 color roles: a restrained green seed, neutral surfaces, tonal selection.
// All components share these roles and a 4-DIP spacing grid; no remote assets.
const (
	controlRadius float32 = 6
	cardRadius    float32 = 12
	dialogRadius  float32 = 14
)

type palette struct {
	background, surface, surfaceLow, surfaceHigh, variant, outline, onSurface, muted ui.Color
	primary, onPrimary, primaryContainer, onContainer, error, errorContainer         ui.Color
}

func colors(dark bool) palette {
	h := ui.Hex
	if dark {
		return palette{
			background: h("#101613"), surface: h("#17211b"), surfaceLow: h("#141d17"), surfaceHigh: h("#233027"), variant: h("#2d3b32"), outline: h("#43564a"), onSurface: h("#dee8df"), muted: h("#afc2b4"),
			primary: h("#96d6b0"), onPrimary: h("#003921"), primaryContainer: h("#185335"), onContainer: h("#b3f2cc"), error: h("#ffb4ab"), errorContainer: h("#5b211f"),
		}
	}
	return palette{
		background: h("#f6f9f6"), surface: h("#ffffff"), surfaceLow: h("#edf3ed"), surfaceHigh: h("#e5eee6"), variant: h("#dce7dd"), outline: h("#c3d1c6"), onSurface: h("#17271d"), muted: h("#53685a"),
		primary: h("#236b45"), onPrimary: h("#ffffff"), primaryContainer: h("#c1efd0"), onContainer: h("#074d2c"), error: h("#ba1a1a"), errorContainer: h("#ffdad6"),
	}
}
func applyTheme(c *ui.Context, dark bool) palette {
	p := colors(dark)
	t := ui.LightTheme()
	if dark {
		t = ui.DarkTheme()
	}
	t.Background, t.Surface, t.SurfaceHover, t.SurfacePressed = p.background, p.surfaceHigh, p.variant, p.outline
	t.Border, t.Text, t.TextMuted = p.outline, p.onSurface, p.muted
	t.Accent, t.AccentText = p.primary, p.onPrimary
	t.AccentHover, t.AccentPressed = p.primary.Mix(p.onSurface, .08), p.primary.Mix(p.onSurface, .14)
	t.Selection, t.Focus = p.primary.Alpha(.22), p.primary
	t.Danger, t.Success = p.error, p.primary
	t.Radius, t.Spacing, t.FontSize = controlRadius, 4, 14
	t.Font = "MiSans" // Uncovered scripts and emoji retain MyGo's system fallback.
	t.InactiveAccent = p.primary
	t.FocusRingWidth, t.FocusRingOffset = 2, 2
	c.SetTheme(t.WithPreferences(c.Preferences()))
	return p
}

func mdButton(c *ui.Context, key, label string, icon *ui.SVG, kind string, disabled bool) ui.Element {
	p := colors(c.Theme().Dark)
	bg, fg := p.surfaceHigh, p.onSurface
	switch kind {
	case "filled":
		bg, fg = p.primary, p.onPrimary
	case "tonal":
		bg, fg = p.primaryContainer, p.onContainer
	case "text":
		bg, fg = ui.Transparent, p.primary
	case "danger":
		bg, fg = p.error, p.background
	case "outline":
		bg, fg = ui.Transparent, p.onSurface
	}
	b := ui.ButtonBase(c.Key(key)).Label(label).Height(40).Padding(0, 14).Gap(8).Radius(controlRadius).
		TextColor(fg).FontSize(14).FontWeight(600).Disabled(disabled).Cursor(ui.CursorPointer)
	if kind == "outline" {
		b.Border(1, p.outline)
	}
	if !disabled {
		if b.Pressed() {
			bg = bg.Mix(fg, .14)
		} else if b.Hovered() {
			bg = bg.Mix(fg, .09)
		}
	}
	animateControl(b.Background(bg), disabled).Children(func() {
		if icon != nil {
			ui.Icon(c, icon).FontSize(20).Shrink(0)
		}
		if label != "" {
			ui.Text(c, label).SingleLine()
		}
	})
	return b
}
func iconButton(c *ui.Context, key, label string, icon *ui.SVG, disabled bool) ui.Element {
	b := ui.ButtonBase(c.Key(key)).Label(label).Tooltip(label).Size(40, 40).Center().Radius(controlRadius).
		Disabled(disabled).Cursor(ui.CursorPointer).TextColor(c.Theme().TextMuted).Background(ui.Transparent)
	if !disabled && (b.Hovered() || b.Pressed()) {
		b.Background(colors(c.Theme().Dark).variant)
	}
	return animateControl(b, disabled).Children(func() { ui.Icon(c, icon).FontSize(22) })
}
func field(c *ui.Context, key, label string, value *string, password bool) ui.Element {
	p := colors(c.Theme().Dark)
	ui.Text(c, label).FontSize(12).FontWeight(600).TextColor(p.muted).Margin(4, 0, 2, 0)
	e := ui.TextInput(c.Key(key), value).Label(label).FillWidth().Height(44).Padding(10, 12).
		Radius(controlRadius).Background(p.surfaceLow).Border(1, p.outline)
	if password {
		e.Password()
	}
	return e
}

// copyField keeps the value selectable and read-only, with a separate action
// at the right of the same outlined surface. Keys do not depend on the locale.
func (a *uiApp) copyField(c *ui.Context, key, label, value string) {
	p := colors(c.Theme().Dark)
	ui.Column(c.Key("copy-field:" + key)).FillWidth().Gap(6).Children(func() {
		ui.Text(c, a.t(label)).FontSize(12).TextColor(p.muted).FontWeight(600)
		ui.Row(c).FillWidth().Height(44).Gap(4).Padding(0, 4, 0, 12).Radius(controlRadius).Background(p.surfaceLow).Border(1, p.outline).Children(func() {
			ui.TextInputBase(c.Key("value"), &value).ReadOnly(true).Label(a.t(label)).Tooltip(value).
				Grow(1).Basis(0).MinWidth(0).Height(40).Padding(10, 0).FontSize(14)
			iconButton(c, "copy", a.t("Copy %s", a.t(label)), iCopy, false).Size(36, 36).Shrink(0).
				Label(a.t("Copy %s", a.t(label))).Tooltip(a.t("Copy %s", a.t(label))).OnClick(func() { a.copy(value, label) })
		})
	})
}

func pill(c *ui.Context, text string, bg, fg ui.Color) {
	ui.Text(c, text).FontSize(11).FontWeight(600).TextColor(fg).Background(bg).Radius(controlRadius).Padding(4, 8).SingleLine().Shrink(0)
}
