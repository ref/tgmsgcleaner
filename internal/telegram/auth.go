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

// ErrEmailSetupRequired signals that Telegram refuses to send a login code
// until a login email is set up for the account, which this app cannot do.
var ErrEmailSetupRequired = errors.New("telegram requires a login email to be set up before it sends a code; set it up in an official Telegram app")

// SentCode is the result of requesting a login code.
type SentCode struct {
	Hash     string // phone_code_hash for SignIn
	Delivery string // human-readable description of where the code was sent
}

func SendCode(ctx context.Context, client *telegram.Client, phone string, sessionPath string) (SentCode, error) {
	res, err := client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		if isAuthRestart(err) {
			_ = os.Remove(sessionPath) // best-effort cleanup before retry
			res, err = client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
		}
		if err != nil {
			return SentCode{}, fmt.Errorf("send code: %w", err)
		}
	}
	sentCode, ok := res.(*tg.AuthSentCode)
	if !ok {
		return SentCode{}, fmt.Errorf("unexpected response type %T", res)
	}
	if _, ok := sentCode.Type.(*tg.AuthSentCodeTypeSetUpEmailRequired); ok {
		return SentCode{}, ErrEmailSetupRequired
	}

	return SentCode{
		Hash:     sentCode.PhoneCodeHash,
		Delivery: describeDelivery(sentCode.Type),
	}, nil
}

// describeDelivery explains where Telegram sent the login code.
func describeDelivery(t tg.AuthSentCodeTypeClass) string {
	switch t := t.(type) {
	case *tg.AuthSentCodeTypeApp:
		return fmt.Sprintf("Code (%d digits) sent to the \"Telegram\" chat in your Telegram app on another device", t.Length)
	case *tg.AuthSentCodeTypeSMS:
		return fmt.Sprintf("Code (%d digits) sent by SMS", t.Length)
	case *tg.AuthSentCodeTypeSMSWord, *tg.AuthSentCodeTypeSMSPhrase, *tg.AuthSentCodeTypeFirebaseSMS:
		return "Code sent by SMS"
	case *tg.AuthSentCodeTypeFragmentSMS:
		return fmt.Sprintf("Code (%d digits) sent to your Fragment number: %s", t.Length, t.URL)
	case *tg.AuthSentCodeTypeCall:
		return fmt.Sprintf("Code (%d digits) will be dictated in a phone call", t.Length)
	case *tg.AuthSentCodeTypeFlashCall:
		return "You will get a flash call; the code is the caller's phone number"
	case *tg.AuthSentCodeTypeMissedCall:
		return fmt.Sprintf("You will get a missed call from a number starting with %s; the code is its last %d digits", t.Prefix, t.Length)
	case *tg.AuthSentCodeTypeEmailCode:
		return fmt.Sprintf("Code (%d digits) sent to your login email %s", t.Length, t.EmailPattern)
	default:
		return fmt.Sprintf("Code sent (delivery type %T)", t)
	}
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
