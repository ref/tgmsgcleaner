package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/ref/tgmsgcleaner/internal/phonenum"
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

func SendCode(ctx context.Context, client *Client, phone string, sessionPath string) (SentCode, error) {
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

// QRProgress reports the state of a QR login.
type QRProgress struct {
	URL  string // tg://login URL to show as a QR code; sent again when the token is refreshed
	Done bool
	Err  error // auth.ErrPasswordAuthNeeded when the account has 2FA enabled
}

// QRLogin shows login tokens on ch until the user accepts one on a device
// where the account is already signed in. It closes ch when it returns.
func QRLogin(ctx context.Context, client *Client, ch chan<- QRProgress) {
	defer close(ch)
	// Drop a signal left over from an earlier attempt: it would make Auth
	// import a token nobody accepted. A token that really was accepted is
	// picked up by the import below.
	select {
	case <-client.loggedIn:
	default:
	}
	qr := client.QR()
	_, err := qr.Auth(ctx, client.loggedIn, func(ctx context.Context, token qrlogin.Token) error {
		select {
		case ch <- QRProgress{URL: token.URL()}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil && ctx.Err() == nil && !tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		// A token accepted while Auth was requesting a new one makes Auth fail
		// on the unexpected "already accepted" reply. Importing completes such
		// a login; when nothing was accepted it fails and the first error stays.
		if _, importErr := qr.Import(ctx); importErr == nil || tgerr.Is(importErr, "SESSION_PASSWORD_NEEDED") {
			err = importErr
		}
	}
	if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		err = auth.ErrPasswordAuthNeeded
	} else if err != nil {
		err = fmt.Errorf("qr login: %w", err)
	}
	select {
	case ch <- QRProgress{Done: true, Err: err}:
	case <-ctx.Done():
	}
}

// ErrAccountMismatch reports that the session was signed in to a different
// account than the one it is saved under, and has been signed out again.
var ErrAccountMismatch = errors.New("signed in to a different account")

// VerifyAccount checks that the session is signed in to the account saved
// under phone. A QR code can be scanned by any account, so without this check
// a session could end up stored under another account's number. On a mismatch
// the session is signed out again, so it cannot be used by mistake, and
// ErrAccountMismatch is returned. Any other error means the check itself
// failed and the session is still signed in.
func VerifyAccount(ctx context.Context, client *Client, phone string) error {
	self, err := client.Self(ctx)
	if err != nil {
		return fmt.Errorf("get self: %w", err)
	}
	if phonenum.Same(self.Phone, phone) {
		return nil
	}
	if _, err := client.API().AuthLogOut(ctx); err != nil {
		return fmt.Errorf("this session belongs to +%s, not %s, and signing it out failed: %w", self.Phone, phone, err)
	}
	return fmt.Errorf("%w: this session belongs to +%s, but the account is saved as %s; "+
		"signed out again — sign in with the right account, or add +%s as a separate account",
		ErrAccountMismatch, self.Phone, phone, self.Phone)
}

func SignIn(ctx context.Context, client *Client, phone, code, codeHash, sessionPath string) error {
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

func CheckPassword(ctx context.Context, client *Client, password, sessionPath string) error {
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
