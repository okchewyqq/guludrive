package workspace

import (
	"guludrive/ui/api"

	"github.com/egoist/mygo/ui"
)

func (a *uiApp) bucketEmail(bucket api.Bucket) string {
	for _, account := range a.snapshot.Accounts {
		if bucket.Album.AccountID != "" && account.ID == bucket.Album.AccountID && account.Email != "" {
			return account.Email
		}
	}
	return "—"
}
func selectionAccent(c *ui.Context, p palette, selected bool) {
	color := ui.Transparent
	if selected {
		color = p.primary
	}
	ui.Box(c).Width(3).Height(24).Radius(1).Shrink(0).Background(color)
}
