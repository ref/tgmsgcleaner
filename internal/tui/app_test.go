package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"

	tg "github.com/anik1ng/tgmsgcleaner/internal/telegram"
)

func newTestApp(t *testing.T) App {
	t.Helper()
	return NewApp(nil, nil, context.Background(), "+70000000000", "/tmp/nonexistent-session", true, 1)
}

func update(t *testing.T, a App, msg any) App {
	t.Helper()
	model, _ := a.Update(msg)
	res, ok := model.(App)
	if !ok {
		t.Fatalf("Update returned %T, want App", model)
	}
	return res
}

func TestStaleMessagesLoadedDiscarded(t *testing.T) {
	a := newTestApp(t)
	a.msgFetchGen = 2
	a.loadingMore = true
	a.nextOffsetID = 500

	// A fetch from a previous view session (gen 1) must be dropped entirely.
	a = update(t, a, messagesLoadedMsg{
		messages:     []tg.MessageInfo{{ID: 1, Text: "stale"}},
		nextOffsetID: 1,
		hasMore:      true,
		gen:          1,
	})
	if len(a.messages) != 0 {
		t.Errorf("stale fetch appended %d messages", len(a.messages))
	}
	if a.nextOffsetID != 500 {
		t.Errorf("stale fetch clobbered nextOffsetID: %d", a.nextOffsetID)
	}

	// The current session's fetch (gen 2) applies normally.
	a = update(t, a, messagesLoadedMsg{
		messages:     []tg.MessageInfo{{ID: 9, Text: "fresh"}},
		nextOffsetID: 9,
		hasMore:      false,
		gen:          2,
	})
	if len(a.messages) != 1 || a.messages[0].Text != "fresh" {
		t.Errorf("current fetch not applied: %v", a.messages)
	}
	if a.nextOffsetID != 9 || a.loadingMore {
		t.Errorf("cursor/loading state wrong: nextOffsetID=%d loadingMore=%v", a.nextOffsetID, a.loadingMore)
	}
}

func TestStaleMessagesErrDiscarded(t *testing.T) {
	a := newTestApp(t)
	a.msgFetchGen = 3
	a.loadingMore = true

	a = update(t, a, messagesErrMsg{err: errors.New("boom"), gen: 2})
	if a.msgErr != "" {
		t.Errorf("stale error surfaced: %q", a.msgErr)
	}
	if !a.loadingMore {
		t.Error("stale error cleared loadingMore of the current session")
	}

	a = update(t, a, messagesErrMsg{err: errors.New("boom"), gen: 3})
	if a.msgErr != "boom" {
		t.Errorf("current error not surfaced: %q", a.msgErr)
	}
	if a.loadingMore {
		t.Error("loadingMore stuck after error")
	}
}

// Raw MTProto chat.ID and channel.ID are independent ID spaces, so two chats
// can legitimately share a numeric ID. Handlers must match on (ID, Type).
func TestDeleteDoneZeroesOnlyMatchingChat(t *testing.T) {
	a := newTestApp(t)
	a.allChats = []tg.ChatInfo{
		{ID: 100, Type: tg.ChatTypeGroup, MyMessages: 5},
		{ID: 100, Type: tg.ChatTypeChannel, MyMessages: 7},
	}
	selected := a.allChats[1]
	a.selectedChat = &selected

	a = update(t, a, deleteProgressMsg{deleted: 7, total: 7, done: true})

	if got := a.allChats[0].MyMessages; got != 5 {
		t.Errorf("basic group with colliding ID was zeroed: MyMessages = %d, want 5", got)
	}
	if got := a.allChats[1].MyMessages; got != 0 {
		t.Errorf("deleted chat count not zeroed: MyMessages = %d, want 0", got)
	}
}

func TestLeftGroupRemovesOnlyMatchingChat(t *testing.T) {
	a := newTestApp(t)
	a.allChats = []tg.ChatInfo{
		{ID: 100, Type: tg.ChatTypeGroup, Title: "group"},
		{ID: 100, Type: tg.ChatTypeSupergroup, Title: "supergroup"},
	}
	selected := a.allChats[1]
	a.selectedChat = &selected

	a = update(t, a, leftGroupMsg{})

	if len(a.allChats) != 1 || a.allChats[0].Type != tg.ChatTypeGroup {
		t.Errorf("wrong chat removed, remaining: %+v", a.allChats)
	}
	if a.selectedChat != nil {
		t.Error("selectedChat not cleared after leaving")
	}
	if a.screen != screenGroupList {
		t.Errorf("screen = %d, want screenGroupList", a.screen)
	}
}

func TestLeftChannelsProgressiveUpdates(t *testing.T) {
	a := newTestApp(t)
	a.allChats = []tg.ChatInfo{
		{ID: 1, Type: tg.ChatTypeSupergroup, Title: "regular", MyMessages: 10},
	}
	a.loadingLeft = true

	// First snapshot: two left channels discovered, counts unknown yet.
	a = update(t, a, leftChannelsMsg{
		chats: []tg.ChatInfo{
			{ID: 50, Type: tg.ChatTypeChannel, Title: "left-a", IsLeft: true, MyMessages: -1},
			{ID: 51, Type: tg.ChatTypeChannel, Title: "left-b", IsLeft: true, MyMessages: -1},
		},
		found: 2,
	})
	if len(a.allChats) != 3 {
		t.Fatalf("after first update: %d chats, want 3", len(a.allChats))
	}
	if a.filter != filterLeftWithMsg {
		t.Errorf("filter = %d, want auto-switch to filterLeftWithMsg", a.filter)
	}
	if a.filterCount() != 6 {
		t.Errorf("filterCount = %d, want 6 once left channels are shown", a.filterCount())
	}

	// Second snapshot supersedes the first: same channels, one count resolved.
	// The left tail must be replaced, not appended again.
	a = update(t, a, leftChannelsMsg{
		chats: []tg.ChatInfo{
			{ID: 50, Type: tg.ChatTypeChannel, Title: "left-a", IsLeft: true, MyMessages: 4},
			{ID: 51, Type: tg.ChatTypeChannel, Title: "left-b", IsLeft: true, MyMessages: -1},
		},
		found:   2,
		counted: 1,
	})
	if len(a.allChats) != 3 {
		t.Fatalf("after second update: %d chats, want 3 (no duplication)", len(a.allChats))
	}
	if a.allChats[0].Title != "regular" || a.allChats[0].MyMessages != 10 {
		t.Errorf("regular chat disturbed: %+v", a.allChats[0])
	}
	if a.allChats[1].MyMessages != 4 {
		t.Errorf("left-a count not updated: %+v", a.allChats[1])
	}

	a = update(t, a, leftChannelsMsg{done: true})
	if a.loadingLeft || !a.leftLoaded {
		t.Errorf("done not applied: loadingLeft=%v leftLoaded=%v", a.loadingLeft, a.leftLoaded)
	}
}

func TestApplyFilterLeftWithMessages(t *testing.T) {
	a := newTestApp(t)
	a.allChats = []tg.ChatInfo{
		{ID: 1, Type: tg.ChatTypeSupergroup, Title: "active", MyMessages: 10},
		{ID: 2, Type: tg.ChatTypeChannel, Title: "left-with-msgs", IsLeft: true, MyMessages: 3},
		{ID: 3, Type: tg.ChatTypeChannel, Title: "left-empty", IsLeft: true, MyMessages: 0},
		{ID: 4, Type: tg.ChatTypeChannel, Title: "left-uncounted", IsLeft: true, MyMessages: -1},
	}

	a.filter = filterLeftWithMsg
	a.applyFilter()

	items := a.list.Items()
	if len(items) != 1 {
		t.Fatalf("Left+msg filter kept %d items, want 1", len(items))
	}
	if g, ok := items[0].(Group); !ok || g.title != "left-with-msgs" {
		t.Errorf("unexpected item: %+v", items[0])
	}

	a.filter = filterLeftAll
	a.applyFilter()
	if got := len(a.list.Items()); got != 3 {
		t.Errorf("Left filter kept %d items, want 3", got)
	}
}

func press(t *testing.T, a App, k tea.KeyPressMsg) App {
	t.Helper()
	return update(t, a, k)
}

func TestPickerDeleteAccount(t *testing.T) {
	keyD := tea.KeyPressMsg{Code: 'd', Text: "d"}
	keyY := tea.KeyPressMsg{Code: 'y', Text: "y"}
	keyEsc := tea.KeyPressMsg{Code: tea.KeyEscape}

	a := NewPickerApp([]string{"111", "222"})

	// 'd' asks for confirmation; esc cancels without exiting.
	a = press(t, a, keyD)
	if a.pickerConfirm != "111" {
		t.Fatalf("pickerConfirm = %q, want %q", a.pickerConfirm, "111")
	}
	a = press(t, a, keyEsc)
	if a.pickerConfirm != "" || a.ExitAction != "" {
		t.Fatalf("esc: pickerConfirm = %q, ExitAction = %q, want both empty", a.pickerConfirm, a.ExitAction)
	}

	// 'd' then 'y' hands the account to main.go for deletion.
	a = press(t, a, keyD)
	a = press(t, a, keyY)
	if a.ExitAction != "delete" || a.Selected != "111" {
		t.Errorf("confirm: ExitAction = %q, Selected = %q, want delete/111", a.ExitAction, a.Selected)
	}
}

func TestPickerAddAccount(t *testing.T) {
	a := NewPickerApp([]string{"111"})
	a = press(t, a, tea.KeyPressMsg{Code: 'a', Text: "a"})
	if a.ExitAction != "add" {
		t.Errorf("ExitAction = %q, want add", a.ExitAction)
	}
}

func newAuthApp(t *testing.T) App {
	t.Helper()
	return NewApp(nil, nil, context.Background(), "+70000000000", "/tmp/nonexistent-session", false, 1)
}

func TestAuthChooseDoesNotRequestCode(t *testing.T) {
	a := newAuthApp(t)
	if cmd := a.Init(); cmd != nil {
		t.Error("Init on the auth screen returned a command; no code should be requested before the user chooses")
	}
	// Typing on the choice screen must not leak into the code input.
	a = press(t, a, tea.KeyPressMsg{Code: '1', Text: "1"})
	if a.authInput.Value() != "" || a.authMode != authModeChoose {
		t.Errorf("input = %q, mode = %d; want empty input and choice mode", a.authInput.Value(), a.authMode)
	}
	a = press(t, a, tea.KeyPressMsg{Code: 'c', Text: "c"})
	if a.authMode != authModeCode {
		t.Errorf("after 'c' mode = %d, want code mode", a.authMode)
	}
	a = press(t, a, tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.authMode != authModeChoose {
		t.Errorf("after esc mode = %d, want choice mode", a.authMode)
	}
}

func TestQRMessages(t *testing.T) {
	a := newAuthApp(t)
	a.authMode = authModeQR
	a.qrGen = 3

	// A token from the current login is rendered and keeps listening.
	model, cmd := a.Update(qrMsg{url: "tg://login?token=abc", gen: 3, next: func() tea.Msg { return nil }})
	a = model.(App)
	if a.qrText == "" || cmd == nil {
		t.Fatalf("qrText empty = %v, cmd nil = %v; want rendered code and next cmd", a.qrText == "", cmd == nil)
	}

	// Messages from a cancelled login are dropped.
	model, cmd = a.Update(qrMsg{done: true, err: errors.New("old"), gen: 2})
	a = model.(App)
	if cmd != nil || a.authErr != "" {
		t.Errorf("stale message changed state: cmd nil = %v, authErr = %q", cmd == nil, a.authErr)
	}

	// A cancelled login must not be treated as a successful sign-in.
	model, cmd = a.Update(qrMsg{done: true, err: context.Canceled, gen: 3})
	a = model.(App)
	if cmd != nil {
		t.Error("cancelled QR login returned a command, want none")
	}

	// A failed login returns to the choice screen with the error shown.
	a.authMode = authModeQR
	a = update(t, a, qrMsg{done: true, err: errors.New("boom"), gen: a.qrGen})
	if a.authMode != authModeChoose || a.authErr != "boom" {
		t.Errorf("mode = %d, authErr = %q; want choice mode and error", a.authMode, a.authErr)
	}
}
