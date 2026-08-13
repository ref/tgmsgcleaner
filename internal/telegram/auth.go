package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ErrAuthRestart signals that Telegram reset the authorization flow
// (AUTH_RESTART). The session file has been removed and the caller must restart
// from SendCode (request a fresh code).
var ErrAuthRestart = errors.New("authorization restarted; request a new code")

func isAuthRestart(err error) bool {
	var rpcErr *tgerr.Error
	return errors.As(err, &rpcErr) && rpcErr.Type == "AUTH_RESTART"
}

func SendCode(ctx context.Context, client *telegram.Client, phone string, sessionPath string) (string, error) {
	res, err := client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		if isAuthRestart(err) {
			_ = os.Remove(sessionPath) // best-effort cleanup before retry
			res, err = client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
		}
		if err != nil {
			return "", fmt.Errorf("send code: %w", err)
		}
	}
	sentCode, ok := res.(*tg.AuthSentCode)
	if !ok {
		return "", fmt.Errorf("unexpected response type")
	}

	return sentCode.PhoneCodeHash, nil
}

func SignIn(ctx context.Context, client *telegram.Client, phone, code, codeHash, sessionPath string) error {
	_, err := client.Auth().SignIn(ctx, phone, code, codeHash)
	if err != nil {
		if isAuthRestart(err) {
			_ = os.Remove(sessionPath)
			return ErrAuthRestart
		}
		return fmt.Errorf("sign in: %w", err)
	}

	return nil
}

func CheckPassword(ctx context.Context, client *telegram.Client, password, sessionPath string) error {
	_, err := client.Auth().Password(ctx, password)
	if err != nil {
		if isAuthRestart(err) {
			_ = os.Remove(sessionPath)
			return ErrAuthRestart
		}
		return fmt.Errorf("check password: %w", err)
	}

	return nil
}
