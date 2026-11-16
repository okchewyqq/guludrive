package workspace

import (
	"strings"

	"guludrive/ui/i18n"
	"guludrive/ui/preferences"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

func matchSystemLocale(tags []string) i18n.Locale {
	for _, tag := range tags {
		language := strings.ToLower(strings.Split(strings.ReplaceAll(tag, "_", "-"), "-")[0])
		switch language {
		case "zh":
			return i18n.Chinese
		case "en":
			return i18n.English
		}
	}
	return i18n.Chinese
}
func (a *uiApp) resolveLocale() {
	a.locale = a.choices.Language
	if a.locale == i18n.System {
		a.locale = a.systemLocale
	}
	a.locale = i18n.Parse(string(a.locale))
}
func (a *uiApp) savePreferences() {
	if a.preferences != nil {
		if err := a.preferences.Save(a.choices); err != nil {
			a.fail(i18n.Err("Could not save appearance preferences; this window will keep your choice."))
		}
	}
}
func (a *uiApp) setTheme(mode preferences.Theme) {
	a.choices.Theme = mode
	mygo.Theme.SetSource(mygo.ThemeSource(mode))
	a.savePreferences()
}
func (a *uiApp) themePicker(c *ui.Context) {
	a.segmentedPicker(c, "settings-theme", true, 40, 14, []pickerItem{
		{key: string(preferences.System), label: a.t("System"), selected: a.choices.Theme == preferences.System, onClick: func() { a.setTheme(preferences.System) }},
		{key: string(preferences.Light), label: a.t("Light theme"), selected: a.choices.Theme == preferences.Light, onClick: func() { a.setTheme(preferences.Light) }},
		{key: string(preferences.Dark), label: a.t("Dark theme"), selected: a.choices.Theme == preferences.Dark, onClick: func() { a.setTheme(preferences.Dark) }},
	})
}
