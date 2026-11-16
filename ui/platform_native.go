package workspace

import (
	"context"
	"os"
	"path/filepath"

	"guludrive/ui/api"
	"guludrive/ui/i18n"
	"guludrive/ui/preferences"

	"github.com/egoist/mygo"
)

func platformPreferences() *preferences.FileStore {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	return &preferences.FileStore{Directory: filepath.Join(dir, "guludrive-ui")}
}
func platformPickFiles(parent *mygo.Window, locale i18n.Locale, done func([]uploadFile, error)) {
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: parent, Title: "GuluDrive · " + i18n.T(locale, "Choose files to upload"), Multiple: true, ButtonLabel: i18n.T(locale, "Review files")})
		if err != nil {
			done(nil, err)
			return
		}
		files := make([]uploadFile, 0, len(paths))
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil {
				done(nil, i18n.Err("Cannot read selected file: %v", err))
				return
			}
			if !info.Mode().IsRegular() {
				done(nil, i18n.Err("%s is not a regular file", info.Name()))
				return
			}
			size := info.Size()
			files = append(files, uploadFile{Name: info.Name(), Size: size, Send: func(ctx context.Context, c *api.Client, dest string, condition api.Preconditions) error {
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				defer f.Close()
				current, err := f.Stat()
				if err != nil {
					return err
				}
				if !current.Mode().IsRegular() || current.Size() != size {
					return i18n.Err("Selected file changed; choose it again before uploading")
				}
				return c.Upload(ctx, dest, f, size, condition)
			}})
		}
		done(files, nil)
	}()
}
