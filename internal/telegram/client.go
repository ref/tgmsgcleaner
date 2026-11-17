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

// Client is a gotd client together with the update signal that QR logins wait
// for, so callers do not have to carry that channel around.
type Client struct {
	*telegram.Client
	loggedIn qrlogin.LoggedIn // signals that a QR login token was accepted on another device
}

// NewClient creates a client with a per-account session file.
func NewClient(apiID int, apiHash string, sessionPath string) *Client {
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
	return &Client{Client: telegram.NewClient(apiID, apiHash, opts), loggedIn: loggedIn}
}

func IsAuthorized(ctx context.Context, client *Client) (bool, error) {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return false, fmt.Errorf("auth status: %w", err)
	}
	return status.Authorized, nil
}
