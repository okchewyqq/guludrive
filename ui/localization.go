package workspace

import (
	"path"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"guludrive/ui/api"
	"guludrive/ui/i18n"
	"guludrive/ui/state"
)

func (a *uiApp) t(id string, args ...any) string { return i18n.T(a.locale, id, args...) }
func (a *uiApp) errorText() string {
	if a.currentError != nil {
		return i18n.ErrorText(a.locale, a.currentError)
	}
	return a.t(a.err)
}
func (a *uiApp) busyText() string        { return a.t(a.busyLabel, a.busyArgs...) }
func (a *uiApp) noticeText() string      { return a.t(a.notice, a.noticeArgs...) }
func (a *uiApp) date(t time.Time) string { return i18n.Modified(a.locale, t) }
func (a *uiApp) bytes(n int64) string {
	if n < 1024 {
		return a.t("%d %s", n, a.t("bytes"))
	}
	return state.Bytes(n)
}
func (a *uiApp) kind(e api.Entry) string {
	if e.IsDir {
		if strings.Count(strings.Trim(e.Path, "/"), "/") == 0 {
			return a.t("Bucket")
		}
		return a.t("Folder")
	}
	if e.ContentType != "" && e.ContentType != "application/octet-stream" {
		return e.ContentType
	}
	ext := strings.TrimPrefix(path.Ext(e.Name), ".")
	if ext != "" {
		return a.t("%s file", strings.ToUpper(ext))
	}
	return a.t("File")
}
func (a *uiApp) button(c *ui.Context, key, label string, icon *ui.SVG, kind string, disabled bool) ui.Element {
	return mdButton(c, key, a.t(label), icon, kind, disabled)
}
func (a *uiApp) iconButton(c *ui.Context, key, label string, icon *ui.SVG, disabled bool) ui.Element {
	return iconButton(c, key, a.t(label), icon, disabled)
}
func (a *uiApp) field(c *ui.Context, key, label string, value *string, password bool) ui.Element {
	return field(c, key, a.t(label), value, password)
}
func (a *uiApp) setLocale(locale i18n.Locale) {
	if !locale.Valid() || a.choices.Language == locale {
		return
	}
	a.choices.Language = locale
	a.resolveLocale()
	a.savePreferences()
}
func (a *uiApp) sortPicker(c *ui.Context) {
	s := ui.SelectBase(c.Key("sort"), &a.order)
	label := map[state.SortOrder]string{state.SortName: "Name", state.SortNewest: "Newest", state.SortSize: "Size"}
	s.Trigger.Label(a.t("Sort files")).Height(44).Width(120).Radius(controlRadius).Padding(0, 12).Background(colors(a.dark).surfaceHigh).Children(func() { ui.Text(c, a.t(label[a.order])); ui.Spacer(c); ui.Icon(c, iChevron).FontSize(14) })
	s.Popup(func(panel ui.Element) {
		panel.Background(colors(a.dark).surface).Radius(controlRadius).Padding(4)
		for _, o := range []state.SortOrder{state.SortName, state.SortNewest, state.SortSize} {
			s.Item(o).Padding(10, 16).Children(func() { ui.Text(c, a.t(label[o])) })
		}
	})
}

type pickerItem struct {
	key, label string
	selected   bool
	onClick    func()
}

// segmentedPicker renders mutually exclusive choices with the caller's control
// height and horizontal padding; wrap keeps the row inside narrow settings cards.
func (a *uiApp) segmentedPicker(c *ui.Context, key string, wrap bool, height, padding float32, items []pickerItem) {
	row := ui.Row(c.Key(key)).Gap(2)
	if wrap {
		row.Wrap()
	}
	row.Children(func() {
		for _, item := range items {
			kind := "text"
			if item.selected {
				kind = "tonal"
			}
			mdButton(c, item.key, item.label, nil, kind, false).Height(height).Padding(0, padding).AccessSelected(item.selected).OnClick(item.onClick)
		}
	})
}
func (a *uiApp) languagePicker(c *ui.Context, key string) {
	a.segmentedPicker(c, key, false, 36, 10, []pickerItem{
		{key: string(i18n.System), label: a.t("System"), selected: a.choices.Language == i18n.System, onClick: func() { a.setLocale(i18n.System) }},
		{key: string(i18n.Chinese), label: "中文", selected: a.choices.Language == i18n.Chinese, onClick: func() { a.setLocale(i18n.Chinese) }},
		{key: string(i18n.English), label: "English", selected: a.choices.Language == i18n.English, onClick: func() { a.setLocale(i18n.English) }},
	})
}
