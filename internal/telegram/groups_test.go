package telegram

import (
	"reflect"
	"testing"

	"github.com/gotd/td/tg"
)

func ownMsg(id int, selfID int64, text string) *tg.Message {
	m := &tg.Message{ID: id, Message: text}
	m.SetFromID(&tg.PeerUser{UserID: selfID})
	return m
}

func TestCollectDeletableIDs(t *testing.T) {
	const selfID int64 = 42

	otherUser := &tg.Message{ID: 5, Message: "not mine"}
	otherUser.SetFromID(&tg.PeerUser{UserID: 777})

	channelAuthor := &tg.Message{ID: 4, Message: "posted as channel"}
	channelAuthor.SetFromID(&tg.PeerChannel{ChannelID: 100})

	noAuthor := &tg.Message{ID: 3, Message: "no from_id"}

	tests := []struct {
		name       string
		raw        []tg.MessageClass
		offsetID   int
		wantIDs    []int
		wantOffset int
	}{
		{
			name:       "empty page keeps cursor",
			raw:        nil,
			offsetID:   99,
			wantIDs:    nil,
			wantOffset: 99,
		},
		{
			name: "only positively own messages are collected",
			raw: []tg.MessageClass{
				ownMsg(10, selfID, "mine"),
				otherUser,
				channelAuthor,
				noAuthor,
				ownMsg(1, selfID, "mine too"),
			},
			offsetID:   20,
			wantIDs:    []int{10, 1},
			wantOffset: 1,
		},
		{
			name: "service and empty messages advance cursor but are never deleted",
			raw: []tg.MessageClass{
				&tg.MessageService{ID: 8},
				&tg.MessageEmpty{ID: 7},
			},
			offsetID:   9,
			wantIDs:    nil,
			wantOffset: 7,
		},
		{
			name: "cursor follows the last raw message even after skips",
			raw: []tg.MessageClass{
				ownMsg(6, selfID, "mine"),
				&tg.MessageService{ID: 2},
			},
			offsetID:   10,
			wantIDs:    []int{6},
			wantOffset: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, next := collectDeletableIDs(tt.raw, selfID, tt.offsetID)
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, tt.wantIDs)
			}
			if next != tt.wantOffset {
				t.Errorf("nextOffset = %d, want %d", next, tt.wantOffset)
			}
		})
	}
}

func TestToMessageInfos(t *testing.T) {
	raw := []tg.MessageClass{
		&tg.Message{ID: 30, Date: 111, Message: "hello"},
		&tg.Message{ID: 20, Date: 222, Message: ""},
		&tg.MessageService{ID: 10},
	}

	infos, next := toMessageInfos(raw, 40)

	want := []MessageInfo{
		{ID: 30, Date: 111, Text: "hello"},
		{ID: 20, Date: 222, Text: "[media]"},
	}
	if !reflect.DeepEqual(infos, want) {
		t.Errorf("infos = %v, want %v", infos, want)
	}
	// The cursor must advance past the trailing service message, or the next
	// page would refetch it forever.
	if next != 10 {
		t.Errorf("nextOffset = %d, want 10", next)
	}

	infos, next = toMessageInfos(nil, 40)
	if infos != nil || next != 40 {
		t.Errorf("empty page: infos = %v, next = %d; want nil, 40", infos, next)
	}
}

func TestHasOwnReaction(t *testing.T) {
	plain := &tg.Message{ID: 1}
	if hasOwnReaction(plain) {
		t.Error("message without reactions reported as having own reaction")
	}

	foreign := &tg.Message{ID: 2}
	foreign.Reactions.Results = []tg.ReactionCount{{Count: 3}}
	if hasOwnReaction(foreign) {
		t.Error("foreign-only reactions reported as own")
	}

	mine := &tg.Message{ID: 3}
	chosen := tg.ReactionCount{Count: 1}
	chosen.SetChosenOrder(0)
	mine.Reactions.Results = []tg.ReactionCount{{Count: 5}, chosen}
	if !hasOwnReaction(mine) {
		t.Error("chosen reaction not detected")
	}
}

func TestSanitizeFilename(t *testing.T) {
	got := sanitizeFilename(`a/b\c:d*e?f"g<h>i|j`)
	want := "a_b_c_d_e_f_g_h_i_j"
	if got != want {
		t.Errorf("sanitizeFilename = %q, want %q", got, want)
	}
	if got := sanitizeFilename("plain name"); got != "plain name" {
		t.Errorf("sanitizeFilename mangled a clean name: %q", got)
	}
}
