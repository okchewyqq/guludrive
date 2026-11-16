package workspace

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"guludrive/assets/fonts"
	"guludrive/ui/api"
	"guludrive/ui/i18n"
	"guludrive/ui/preferences"
	"guludrive/ui/state"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

type snapshot struct {
	Status    api.Status
	Accounts  []api.Account
	Buckets   []api.Bucket
	Listing   api.Listing
	Downloads api.DownloadSettings
	Admin     api.AdminProfile
}
type uploadFile struct {
	Name string
	Size int64
	Send func(context.Context, *api.Client, string, api.Preconditions) error
}
type dialogState struct {
	Open                 bool
	Kind                 string
	Value                string
	Target               api.Entry
	Targets              []api.Entry
	Account              api.Account
	Overwrite, Recursive bool
}

const defaultServerURL = "http://127.0.0.1:8787"

// Options configures the native HTTP client. Credentials are process-memory only.
// Context ends the window and its HTTP work when the host service stops.
// ServerURL is the co-hosted default; a saved preference overrides it.
type Options struct {
	Context     context.Context
	ServerURL   string
	Preferences *preferences.FileStore
	Language    i18n.Locale
}
type uiApp struct {
	locale                            i18n.Locale
	choices                           preferences.Choices
	systemLocale                      i18n.Locale
	setupChecked, initialized         bool
	confirmPassword                   string
	proxyEpoch                        uint64
	proxyCancel                       context.CancelFunc
	scriptBusy                        bool
	publicOrigin                      string
	preferences                       *preferences.FileStore
	currentError                      error
	busyArgs, noticeArgs              []any
	ctx                               context.Context
	cancel                            context.CancelFunc
	dispatch                          func(func())
	clipboardWrite                    func(value, description string) error
	window                            *mygo.Window
	client                            *api.Client
	snapshot                          snapshot
	connected, busy                   bool
	busyLabel, err, notice            string
	activeCancel                      context.CancelFunc
	path, query, page                 string
	selected                          map[string]bool
	order                             state.SortOrder
	list                              ui.ListState
	dark                              bool
	server, username, password, oauth string
	// serverTarget is the address the current client was built for.
	serverTarget                   string
	secretEpoch                    int
	dialog                         dialogState
	uploads                        []uploadFile
	downloadMode, downloadApplying api.DownloadMode
	gdirectRefreshing              bool
	gdirectEpoch                   uint64
	gdirectPollCancel              context.CancelFunc
	workerURL, workerSecret        string
	admin                          adminForm
	uploadSettings                 uploadSettingsForm
	operationEpoch                 uint64
}

func newUI(opts Options) *uiApp {
	parent := opts.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	choices := preferences.Defaults()
	if opts.Language.Valid() {
		choices.Language = opts.Language
	}
	if opts.Preferences != nil {
		if saved, err := opts.Preferences.Load(); err == nil && saved.Valid() {
			choices = saved
		}
	}
	language := i18n.Parse(string(choices.Language))
	server := opts.ServerURL
	if server == "" {
		server = choices.Server
	}
	if server == "" {
		server = defaultServerURL
	}
	return &uiApp{ctx: ctx, cancel: cancel, path: "/", order: state.SortName, page: "Files", server: server, serverTarget: server, username: "admin",
		locale: language, preferences: opts.Preferences, choices: choices, systemLocale: i18n.Chinese}
}

// windowIconPNG is the product mark (web/public/icon.svg), rasterised once.
//
//go:embed icon.png
var windowIconPNG []byte

var registerFontsOnce sync.Once
var registerFontsError error

// Run starts the native MyGo window on the caller's main goroutine.
func Run(opts Options) error {
	if opts.Context != nil && opts.Context.Err() != nil {
		return nil
	}
	registerFontsOnce.Do(func() {
		for _, face := range [][]byte{fonts.MiSans, fonts.MiSansMedium, fonts.MiSansBold} {
			if err := ui.RegisterFont(face, "MiSans"); err != nil {
				registerFontsError = fmt.Errorf("register MiSans font: %w", err)
				return
			}
		}
	})
	if registerFontsError != nil {
		return registerFontsError
	}
	if opts.Preferences == nil {
		opts.Preferences = platformPreferences()
	}
	a := newUI(opts)
	defer a.cancel()
	defer a.cancelAdminEdit()
	defer a.stopGDirectPoll()
	defer a.stopProxyWork()
	mygo.Theme.SetSource(mygo.ThemeSource(a.choices.Theme))
	mygo.App.SetName("GuluDrive")
	mygo.App.WhenReady(func() {
		if a.ctx.Err() != nil {
			mygo.App.Quit()
			return
		}
		// MyGo reads native preferred UI languages (Windows MUI, macOS or Linux locale).
		a.systemLocale = matchSystemLocale(mygo.App.PreferredLanguages())
		a.resolveLocale()
		// The view draws the title bar and its window controls itself: a
		// hidden title bar without the system's controls keeps the shadow,
		// the rounded corners and the resize edges, the top one included.
		win := mygo.NewWindow(mygo.WindowOptions{
			Title: "GuluDrive", Width: 1240, Height: 820, MinWidth: 620, MinHeight: 520, Content: ui.View(a.view),
			TitleBarStyle: mygo.TitleBarHidden, HideWindowButtons: true, TitleBarHeight: 44,
		})
		a.window = win
		// The icon is cosmetic: a backend that takes none keeps the default.
		_ = win.SetIcon(windowIconPNG)
		// The drawn maximize glyph follows the window's state. The callbacks
		// run on the main loop, which Invalidate is safe to redraw from.
		for _, listen := range []func(func()) func(){
			win.OnMaximize, win.OnUnmaximize, win.OnResize,
		} {
			listen(func() { win.Invalidate() })
		}
		a.dispatch = win.Update
		a.checkSetup()
		go func() {
			<-a.ctx.Done()
			// MyGo documents App.Quit as goroutine-safe; it posts to the
			// main loop so the host never drives native UI from its worker.
			mygo.App.Quit()
		}()
	})
	return mygo.App.Run()
}
func (a *uiApp) fail(err error) {
	if err == nil {
		return
	}
	a.err = err.Error()
	a.currentError = err
}

// All model writes happen on MyGo's main thread. The work closure captures
// immutable inputs, never a build-lifetime ui.Element or *ui.Context.
func (a *uiApp) launch(label string, work func(context.Context) (func(), error)) {
	if a.busy || a.dispatch == nil || a.ctx.Err() != nil {
		return
	}
	a.busy = true
	a.busyLabel = label
	a.busyArgs = nil
	a.noticeArgs = nil
	a.err = ""
	a.notice = ""
	ctx, cancel := context.WithCancel(a.ctx)
	a.activeCancel = cancel
	a.operationEpoch++
	epoch := a.operationEpoch
	c, p, wasConnected := a.client, a.path, a.connected
	go func() {
		apply, err := work(ctx)
		stale := api.PreconditionFailed(err) && wasConnected && c != nil
		var latest snapshot
		var refreshErr error
		if stale {
			latest, refreshErr = load(ctx, c, p)
		}
		requestCancelled := ctx.Err() != nil
		cancel()
		a.dispatch(func() {
			if epoch != a.operationEpoch {
				return
			}
			if a.ctx.Err() != nil {
				a.cancelAdminEdit()
				a.busy = false
				a.activeCancel = nil
				return
			}
			if requestCancelled {
				a.downloadApplying = ""
				a.gdirectRefreshing = false
				apply = nil
				stale = false
				err = context.Canceled
			}
			a.busy = false
			a.activeCancel = nil
			if apply != nil {
				apply()
			}
			if stale {
				a.closeDialog()
				a.clearSelection()
				if refreshErr == nil {
					a.accept(latest, p)
					a.fail(i18n.Err("The item changed on the server (HTTP 412). The directory was refreshed. Review it and confirm again; no operation was forced or retried"))
				} else {
					a.fail(i18n.Err("The item changed on the server (HTTP 412). Refresh failed: %v. Refresh manually before trying again", refreshErr))
				}
				return
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					a.fail(i18n.Err("Operation cancelled. Refresh to check whether the server committed a change"))
				} else {
					a.fail(err)
				}
				if api.Unauthorized(err) {
					a.clearSession()
				}
			}
		})
	}()
}
func load(ctx context.Context, c *api.Client, p string) (snapshot, error) {
	var s snapshot
	var err error
	if s.Status, err = c.Status(ctx); err != nil {
		return s, err
	}
	if s.Admin, err = c.AdminProfile(ctx); err != nil {
		return s, err
	}
	if s.Downloads, err = c.DownloadSettings(ctx); err != nil {
		return s, err
	}
	if s.Accounts, err = c.Accounts(ctx); err != nil {
		return s, err
	}
	if s.Buckets, err = c.Buckets(ctx); err != nil {
		return s, err
	}
	if s.Listing, err = c.Files(ctx, p); err != nil {
		return s, err
	}
	return s, nil
}
func (a *uiApp) accept(s snapshot, p string) {
	a.snapshot = s
	if a.page != "Settings" {
		a.downloadMode, a.workerURL, a.publicOrigin = s.Downloads.Mode, s.Downloads.WorkerURL, s.Downloads.PublicOrigin
	}
	a.startGDirectPoll()
	a.path = p
	present := make(map[string]bool, len(s.Listing.Entries))
	for _, e := range s.Listing.Entries {
		present[e.Path] = true
	}
	for selected := range a.selected {
		if !present[selected] {
			delete(a.selected, selected)
		}
	}
}

// normalizeServer trims the surrounding space and trailing slash the API client drops too.
func normalizeServer(value string) string { return strings.TrimRight(strings.TrimSpace(value), "/") }

// resetConnection drops everything bound to the previous server. Sessions stay
// in the client's cookie jar, so a new address never reuses a session.
func (a *uiApp) resetConnection() {
	a.stopProxyWork()
	a.stopGDirectPoll()
	a.operationEpoch++
	if a.activeCancel != nil {
		a.activeCancel()
	}
	a.activeCancel = nil
	a.busy = false
	a.client = nil
	a.serverTarget = ""
	a.connected, a.setupChecked, a.initialized = false, false, false
	a.password, a.confirmPassword = "", ""
	a.secretEpoch++
	a.snapshot = snapshot{}
	a.clearSelection()
}

// applyServer persists the edited address and rebuilds the client when it moved.
func (a *uiApp) applyServer() error {
	server := normalizeServer(a.server)
	if server == a.serverTarget {
		a.server = server
		return nil
	}
	if !preferences.ValidServerURL(server) {
		return i18n.Err("Enter an absolute http:// or https:// server URL")
	}
	c, err := api.New(server, nil)
	if err != nil {
		return err
	}
	if server != a.choices.Server {
		a.choices.Server = server
		a.savePreferences()
	}
	a.server = server
	a.resetConnection()
	a.client, a.serverTarget = c, c.BaseURL()
	return nil
}

// switchServer returns to the connection view without carrying a session over.
func (a *uiApp) switchServer() {
	if a.busy {
		return
	}
	a.err, a.notice = "", ""
	a.noticeArgs = nil
	a.resetConnection()
	a.setPage("Files")
}
func (a *uiApp) connect() {
	if a.busy {
		return
	}
	if err := a.applyServer(); err != nil {
		a.fail(err)
		return
	}
	c := a.client
	if c == nil {
		var err error
		c, err = api.New(a.server, nil)
		if err != nil {
			a.fail(err)
			return
		}
		a.serverTarget = c.BaseURL()
	}
	if !a.setupChecked {
		a.checkSetup()
		return
	}
	if !a.initialized {
		a.initializeAdmin()
		return
	}
	if a.username == "" || a.password == "" {
		a.fail(i18n.Err("Enter your administrator username and password"))
		return
	}
	username, password := a.username, a.password
	a.password = ""
	a.secretEpoch++ // Re-key to discard the secret editor's undo history.
	a.launch("Connecting securely", func(ctx context.Context) (func(), error) {
		status, err := c.GetSetup(ctx)
		if err != nil {
			return nil, err
		}
		if !status.Initialized {
			return func() { a.client = c; a.setupChecked = true; a.initialized = false }, nil
		}
		if err := c.Login(ctx, username, password); err != nil {
			return nil, err
		}
		s, err := load(ctx, c, "/")
		if err != nil {
			return nil, err
		}
		return func() { a.client = c; a.connected = true; a.clearSelection(); a.accept(s, "/") }, nil
	})
}

func (a *uiApp) refresh() { a.browse(a.path, false) }
func (a *uiApp) browse(p string, navigate bool) {
	if !a.connected || a.busy {
		return
	}
	clean, err := state.Normalize(p)
	if err != nil {
		a.fail(err)
		return
	}
	c := a.client
	a.launch("Refreshing your workspace", func(ctx context.Context) (func(), error) {
		s, err := load(ctx, c, clean)
		if err != nil {
			return nil, err
		}
		return func() {
			if a.path != clean {
				a.list = ui.ListState{}
				a.query = ""
				a.clearSelection()
			}
			a.accept(s, clean)
			if navigate {
				a.setPage("Files")
			}
		}, nil
	})
}

// A batch may have committed earlier files before a later request fails.
// Preserve the error, but refresh the published directory without retrying it.
type partialMutationError struct{ err error }

func (e *partialMutationError) Error() string                  { return e.err.Error() }
func (e *partialMutationError) Unwrap() error                  { return e.err }
func (e *partialMutationError) Localized(l i18n.Locale) string { return i18n.ErrorText(l, e.err) }
func (a *uiApp) mutate(label string, fn func(context.Context, *api.Client) error) {
	c, p := a.client, a.path
	a.launch(label, func(ctx context.Context) (func(), error) {
		if err := fn(ctx, c); err != nil {
			var partial *partialMutationError
			if !errors.As(err, &partial) || api.PreconditionFailed(err) || ctx.Err() != nil {
				return nil, err
			}
			latest, refreshErr := load(ctx, c, p)
			if refreshErr != nil {
				return nil, errors.Join(err, i18n.Err("Refresh after partial completion failed: %v", refreshErr))
			}
			return func() { a.accept(latest, p) }, err
		}
		s, err := load(ctx, c, p)
		return func() {
			a.closeDialog()
			a.clearSelection()
			a.notice = "%s completed"
			a.noticeArgs = []any{&i18n.Message{ID: label}}
			if err == nil {
				a.accept(s, p)
			}
		}, err
	})
}
func (a *uiApp) selectAccount(id string) {
	a.mutate("Select account", func(ctx context.Context, s *api.Client) error { return s.SelectAccount(ctx, id) })
}
func (a *uiApp) clearSelection() { a.selected = nil }
func (a *uiApp) toggleSelection(p string) {
	if a.selected[p] {
		delete(a.selected, p)
		return
	}
	if a.selected == nil {
		a.selected = make(map[string]bool)
	}
	a.selected[p] = true
}
func (a *uiApp) selection() (api.Entry, bool) {
	if len(a.selected) != 1 {
		return api.Entry{}, false
	}
	for _, e := range a.snapshot.Listing.Entries {
		if a.selected[e.Path] {
			return e, true
		}
	}
	return api.Entry{}, false
}
func (a *uiApp) selectedEntries() []api.Entry {
	entries := make([]api.Entry, 0, len(a.selected))
	// Match the visible file-list ordering, but include selected items hidden by search.
	for _, e := range state.Entries(a.snapshot.Listing.Entries, "", a.order) {
		if a.selected[e.Path] {
			entries = append(entries, e)
		}
	}
	return entries
}
func (a *uiApp) selectedFiles() []api.Entry {
	entries := a.selectedEntries()
	files := make([]api.Entry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			files = append(files, e)
		}
	}
	return files
}
func (a *uiApp) openDialog(kind string, target api.Entry) {
	if a.busy {
		return
	}
	a.closeDialog()
	a.err, a.notice = "", ""
	a.noticeArgs = nil
	a.dialog = dialogState{Open: true, Kind: kind, Target: target, Recursive: target.IsDir}
	switch kind {
	case "rename":
		a.dialog.Value = target.Name
	case "move", "copy":
		a.dialog.Value = target.Path
	case "upload":
		a.dialog.Value = a.path
	}
}

// A batch has no single source path, so its destination field starts empty.
func (a *uiApp) openBatchDialog(kind string, targets []api.Entry) {
	if a.busy {
		return
	}
	a.closeDialog()
	a.err, a.notice = "", ""
	a.noticeArgs = nil
	a.dialog = dialogState{Open: true, Kind: kind, Targets: targets}
}
func (a *uiApp) closeDialog() {
	a.dialog = dialogState{}
	a.oauth = ""
	a.secretEpoch++
	a.uploads = nil
}
func (a *uiApp) submitDialog() {
	if a.busy {
		return
	}
	d := a.dialog
	if len(d.Targets) > 1 && (d.Kind == "move" || d.Kind == "copy" || d.Kind == "delete") {
		a.submitBatch(d)
		return
	}
	switch d.Kind {
	case "bucket":
		if err := state.Name(d.Value); err != nil {
			a.fail(err)
			return
		}
		a.mutate("Create bucket", func(ctx context.Context, c *api.Client) error { return c.CreateBucket(ctx, d.Value) })
	case "directory":
		p, err := state.Join(a.path, d.Value)
		if err != nil {
			a.fail(err)
			return
		}
		a.mutate("Create folder", func(ctx context.Context, c *api.Client) error { return c.CreateDirectory(ctx, p) })
	case "rename", "move", "copy":
		var dest string
		var err error
		if d.Kind == "rename" {
			dest, err = state.Join(path.Dir(d.Target.Path), d.Value)
		} else {
			dest, err = state.Destination(a.path, d.Value)
		}
		if err != nil {
			a.fail(err)
			return
		}
		if dest == d.Target.Path {
			a.fail(i18n.Err("The destination must be different from the source"))
			return
		}
		if dest == "/" {
			a.fail(i18n.Err("The workspace root cannot be replaced"))
			return
		}
		v := api.Transfer{Source: d.Target.Path, Destination: dest, Overwrite: d.Overwrite, Recursive: d.Recursive}
		condition := api.Preconditions{IfMatch: d.Target.ETag()}
		a.mutate(map[string]string{"rename": "Rename", "move": "Move", "copy": "Copy"}[d.Kind], func(ctx context.Context, c *api.Client) error {
			if d.Kind == "copy" {
				return c.Copy(ctx, v, condition)
			}
			return c.Move(ctx, v, condition)
		})
	case "delete":
		a.mutate("Delete", func(ctx context.Context, c *api.Client) error {
			return c.Delete(ctx, d.Target.Path, api.Preconditions{IfMatch: d.Target.ETag()})
		})
	case "google":
		token := a.oauth
		a.oauth = ""
		a.secretEpoch++
		if token == "" {
			a.fail(i18n.Err("Paste the oauth_token copied from Google's login page"))
			return
		}
		a.mutate("Add Google account", func(ctx context.Context, c *api.Client) error { _, err := c.AddGoogle(ctx, token); return err })
	case "remove-account":
		a.mutate("Remove account", func(ctx context.Context, c *api.Client) error { return c.RemoveAccount(ctx, d.Account.ID) })
	case "upload":
		a.submitUploads()
	}
}

// Batch move/copy/delete reuse the single-entry API one request at a time;
// the server exposes no batch endpoint.
func (a *uiApp) submitBatch(d dialogState) {
	type batchItem struct {
		entry api.Entry
		dest  string
	}
	label := map[string]string{"move": "Move", "copy": "Copy", "delete": "Delete"}[d.Kind]
	busy := map[string]string{"move": "Moving %d of %d · %s", "copy": "Copying %d of %d · %s", "delete": "Deleting %d of %d · %s"}[d.Kind]
	items := make([]batchItem, len(d.Targets))
	if d.Kind != "delete" {
		directory, err := state.Normalize(d.Value)
		if err != nil {
			a.fail(err)
			return
		}
		if directory == "/" {
			a.fail(i18n.Err("The workspace root cannot be replaced"))
			return
		}
		// Resolve every destination before the first request can change the server.
		for i, e := range d.Targets {
			dest, err := state.Join(directory, e.Name)
			if err != nil {
				a.fail(err)
				return
			}
			if dest == e.Path {
				a.fail(i18n.Err("The destination must be different from the source"))
				return
			}
			items[i] = batchItem{entry: e, dest: dest}
		}
	} else {
		for i, e := range d.Targets {
			items[i] = batchItem{entry: e}
		}
	}
	overwrite := d.Overwrite
	a.mutate(label, func(ctx context.Context, c *api.Client) error {
		for i, item := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			e := item.entry
			a.dispatch(func() { a.busyLabel = busy; a.busyArgs = []any{i + 1, len(items), e.Name} })
			condition := api.Preconditions{IfMatch: e.ETag()}
			v := api.Transfer{Source: e.Path, Destination: item.dest, Overwrite: overwrite, Recursive: e.IsDir}
			var err error
			switch d.Kind {
			case "move":
				err = c.Move(ctx, v, condition)
			case "copy":
				err = c.Copy(ctx, v, condition)
			default:
				err = c.Delete(ctx, e.Path, condition)
			}
			if err != nil {
				if i == 0 {
					return err
				}
				failure := i18n.Err("Batch operation stopped at %q (earlier entries may have finished): %v", e.Name, err)
				return &partialMutationError{err: failure}
			}
		}
		return nil
	})
}
func (a *uiApp) pickUploads() {
	if a.busy {
		return
	}
	platformPickFiles(a.window, a.locale, func(files []uploadFile, err error) {
		a.dispatch(func() {
			if err != nil {
				a.fail(err)
				return
			}
			if len(files) == 0 {
				return
			}
			a.openDialog("upload", api.Entry{})
			a.uploads = files
		})
	})
}
func (a *uiApp) submitUploads() {
	directory, err := state.Normalize(a.dialog.Value)
	if err != nil {
		a.fail(err)
		return
	}
	if directory == "/" {
		a.fail(i18n.Err("Choose a bucket or a folder inside a bucket before uploading"))
		return
	}
	files := append([]uploadFile(nil), a.uploads...)
	if len(files) == 0 {
		a.fail(i18n.Err("Choose at least one file"))
		return
	}
	for _, f := range files {
		if err := state.Name(f.Name); err != nil {
			a.fail(i18n.Err("%s: %v", f.Name, err))
			return
		}
	}
	replace := a.dialog.Overwrite
	a.mutate("Upload files", func(ctx context.Context, c *api.Client) error {
		// Preflight all names before the first upload: PUT can replace files.
		existing, err := c.Files(ctx, directory)
		if err != nil {
			return err
		}
		if !existing.Entry.IsDir {
			return i18n.Err("The upload destination must be a folder inside a bucket")
		}
		names := map[string]api.Entry{}
		for _, e := range existing.Entries {
			names[e.Name] = e
		}
		chosen := map[string]bool{}
		for _, f := range files {
			if chosen[f.Name] {
				return i18n.Err("The selection contains duplicate filename %q", f.Name)
			}
			chosen[f.Name] = true
			if target, exists := names[f.Name]; exists {
				if target.IsDir {
					return i18n.Err("%q is a folder and cannot be replaced by an uploaded file", f.Name)
				}
				if !replace {
					return i18n.Err("%q already exists. Enable Replace existing files to continue", f.Name)
				}
			}
		}
		for i, f := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			a.dispatch(func() { a.busyLabel = "Uploading %d of %d · %s"; a.busyArgs = []any{i + 1, len(files), f.Name} })
			dest, _ := state.Join(directory, f.Name)
			condition := api.Preconditions{IfNoneMatch: "*"}
			if target, exists := names[f.Name]; exists {
				condition = api.Preconditions{IfMatch: target.ETag()}
			}
			if err := f.Send(ctx, c, dest, condition); err != nil {
				if i == 0 {
					return err
				}
				failure := i18n.Err("Upload stopped at %q (earlier files may have finished): %v", f.Name, err)
				return &partialMutationError{err: failure}
			}
		}
		return nil
	})
}
func (a *uiApp) clearSession() {
	a.stopProxyWork()
	a.stopUploadSettings()
	a.uploadSettings = uploadSettingsForm{epoch: a.uploadSettings.epoch}
	a.stopGDirectPoll()
	a.operationEpoch++
	if a.activeCancel != nil {
		a.activeCancel()
	}
	a.activeCancel = nil
	a.busy = false
	a.cancelAdminEdit()
	a.admin.Username = ""
	a.path, a.page, a.query = "/", "Files", ""
	a.list = ui.ListState{}
	a.connected = false
	a.client = nil
	a.snapshot = snapshot{}
	a.clearSelection()
	a.password, a.confirmPassword, a.workerURL, a.workerSecret, a.publicOrigin = "", "", "", "", ""
	a.downloadMode, a.downloadApplying = "", ""
	a.gdirectRefreshing = false
	a.closeDialog()
}

func (a *uiApp) logout() {
	if a.busy || a.client == nil {
		return
	}
	c := a.client
	a.launch("Sign out", func(ctx context.Context) (func(), error) {
		if err := c.Logout(ctx); err != nil && !api.Unauthorized(err) {
			return nil, err
		}
		return func() { a.clearSession(); a.notice = "Signed out" }, nil
	})
}
