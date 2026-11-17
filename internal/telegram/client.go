// Package telegram wraps gotd/td client for auth, messaging and chat operations.
package telegram

import (
	"context"
	"fmt"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
)

// NewClient creates a client with a per-account session file. The returned
// channel signals when a QR login token was accepted on another device.
func NewClient(apiID int, apiHash string, sessionPath string) (*telegram.Client, qrlogin.LoggedIn) {
	dispatcher := tg.NewUpdateDispatcher()
	// Buffered, unlike qrlogin.OnLoginToken: the update can arrive while the QR
	// login is busy refreshing its token, and an unbuffered signal sent at that
	// moment would be lost.
	loggedIn := make(chan struct{}, 1)
	dispatcher.OnLoginToken(func(context.Context, tg.Entities, *tg.UpdateLoginToken) error {
		select {
		case loggedIn <- struct{}{}:
		default:
		}
		return nil
	})
	opts := telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath},
		UpdateHandler:  dispatcher,
	}
	return telegram.NewClient(apiID, apiHash, opts), loggedIn
}

func IsAuthorized(ctx context.Context, client *telegram.Client) (bool, error) {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return false, fmt.Errorf("auth status: %w", err)
	}
	return status.Authorized, nil
}
