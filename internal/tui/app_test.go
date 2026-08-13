package tui

import (
	"context"
	"errors"
	"testing"

	tg "github.com/anik1ng/tgmsgcleaner/internal/telegram"
)

func newTestApp(t *testing.T) App {
	t.Helper()
	return NewApp(nil, context.Background(), "+70000000000", "/tmp/nonexistent-session", true, 1)
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
