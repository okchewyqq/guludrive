package workspace

import (
	"context"
	"errors"
	"net/http"

	"guludrive/ui/api"
	"guludrive/ui/i18n"
)

func (a *uiApp) checkSetup() {
	if a.busy {
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
	a.launch("Connecting securely", func(ctx context.Context) (func(), error) {
		setup, err := c.GetSetup(ctx)
		if err != nil {
			return nil, err
		}
		return func() { a.client = c; a.setupChecked = true; a.initialized = setup.Initialized }, nil
	})
}
func (a *uiApp) initializeAdmin() {
	if a.busy || !a.setupChecked || a.initialized || a.client == nil {
		return
	}
	username, password, confirm := a.username, a.password, a.confirmPassword
	a.password, a.confirmPassword = "", ""
	a.secretEpoch++
	if err := api.ValidateInitialAdmin(username, password); err != nil {
		a.fail(err)
		return
	}
	if password != confirm {
		a.fail(i18n.Err("The new password and confirmation do not match"))
		return
	}
	c := a.client
	a.launch("Create administrator", func(ctx context.Context) (func(), error) {
		if err := c.Initialize(ctx, username, password); err != nil {
			var remote *api.Error
			if errors.As(err, &remote) && remote.Status == http.StatusConflict {
				return func() { a.initialized = true }, err
			}
			return nil, err
		}
		if err := c.Login(ctx, username, password); err != nil {
			return func() { a.initialized = true }, err
		}
		s, err := load(ctx, c, "/")
		if err != nil {
			return func() { a.initialized = true }, err
		}
		return func() { a.initialized = true; a.client = c; a.connected = true; a.clearSelection(); a.accept(s, "/") }, nil
	})
}
