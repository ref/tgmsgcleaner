package telegram

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type ChatType string

const (
	ChatTypeGroup      ChatType = "group"
	ChatTypeSupergroup ChatType = "supergroup"
	ChatTypeChannel    ChatType = "channel"
)

type ChatInfo struct {
	Title       string
	ID          int64
	Type        ChatType
	MemberCount int
	Username    string
	InputPeer   tg.InputPeerClass
	MyMessages  int
	IsLeft      bool
}

func GetChats(ctx context.Context, client *telegram.Client) ([]ChatInfo, error) {
	api := client.API()

	var allChats []ChatInfo
	// Separate dedup spaces: raw MTProto chat.ID (basic groups) and channel.ID
	// (channels/supergroups) are independent, so the same number in each is a
	// different chat and must not collide.
	seenChats := make(map[int64]bool)
	seenChannels := make(map[int64]bool)
	offsetDate := 0
	offsetID := 0
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}

	for {
		result, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      100,
		})
		if err != nil {
			return nil, fmt.Errorf("getting dialogs: %w", err)
		}

		var dialogs []tg.DialogClass
		var rawChats []tg.ChatClass
		var rawUsers []tg.UserClass
		var messages []tg.MessageClass
		var isComplete bool

		switch d := result.(type) {
		case *tg.MessagesDialogs:
			dialogs = d.Dialogs
			rawChats = d.Chats
			rawUsers = d.Users
			messages = d.Messages
			isComplete = true
		case *tg.MessagesDialogsSlice:
			dialogs = d.Dialogs
			rawChats = d.Chats
			rawUsers = d.Users
			messages = d.Messages
			isComplete = len(d.Dialogs) < 100
		default:
			isComplete = true
		}

		channelMap := make(map[int64]*tg.Channel)
		for _, c := range rawChats {
			switch chat := c.(type) {
			case *tg.Chat:
				id := int64(chat.ID)
				if seenChats[id] || chat.Deactivated || chat.Left {
					continue
				}
				seenChats[id] = true
				allChats = append(allChats, ChatInfo{
					Title:       chat.Title,
					ID:          id,
					Type:        ChatTypeGroup,
					MemberCount: chat.ParticipantsCount,
					InputPeer:   &tg.InputPeerChat{ChatID: chat.ID},
					MyMessages:  -1,
				})
			case *tg.Channel:
				channelMap[chat.ID] = chat
				if seenChannels[chat.ID] || chat.Left {
					continue
				}
				seenChannels[chat.ID] = true
				ct := ChatTypeChannel
				if chat.Megagroup {
					ct = ChatTypeSupergroup
				}
				username := ""
				if u, ok := chat.GetUsername(); ok {
					username = u
				}
				allChats = append(allChats, ChatInfo{
					Title:       chat.Title,
					ID:          chat.ID,
					Type:        ct,
					MemberCount: chat.ParticipantsCount,
					Username:    username,
					InputPeer:   &tg.InputPeerChannel{ChannelID: chat.ID, AccessHash: chat.AccessHash},
					MyMessages:  -1,
				})
			}
		}

		userMap := make(map[int64]*tg.User)
		for _, u := range rawUsers {
			if user, ok := u.(*tg.User); ok {
				userMap[user.ID] = user
			}
		}

		if isComplete || len(dialogs) == 0 {
			break
		}

		lastDialog := dialogs[len(dialogs)-1]
		d, ok := lastDialog.(*tg.Dialog)
		if !ok {
			break
		}

		topMsgID := d.TopMessage
		foundOffset := false
		for _, m := range messages {
			if msg, ok := m.(*tg.Message); ok && msg.ID == topMsgID {
				offsetDate = msg.Date
				offsetID = msg.ID
				switch p := d.Peer.(type) {
				case *tg.PeerUser:
					if user, ok := userMap[p.UserID]; ok {
						offsetPeer = &tg.InputPeerUser{UserID: p.UserID, AccessHash: user.AccessHash}
					} else {
						offsetPeer = &tg.InputPeerUser{UserID: p.UserID}
					}
				case *tg.PeerChat:
					offsetPeer = &tg.InputPeerChat{ChatID: p.ChatID}
				case *tg.PeerChannel:
					if ch, ok := channelMap[p.ChannelID]; ok {
						offsetPeer = &tg.InputPeerChannel{ChannelID: p.ChannelID, AccessHash: ch.AccessHash}
					} else {
						offsetPeer = &tg.InputPeerChannel{ChannelID: p.ChannelID}
					}
				}
				foundOffset = true
				break
			}
		}
		if !foundOffset {
			break
		}
	}

	return allChats, nil
}

func CountMyMessages(ctx context.Context, client *telegram.Client, peer tg.InputPeerClass, selfID int64) (int, error) {
	// Delegate to the retry-safe helper (handles FLOOD_WAIT via invokeWithRetry).
	return countMessages(ctx, client.API(), peer, selfID, 0)
}

func GetSelfID(ctx context.Context, client *telegram.Client) (int64, error) {
	me, err := client.Self(ctx)
	if err != nil {
		return 0, fmt.Errorf("get self: %w", err)
	}
	return me.ID, nil
}

type MessageInfo struct {
	ID   int
	Date int
	Text string
}

func FetchMyMessages(ctx context.Context, client *telegram.Client, chat ChatInfo, selfID int64, offsetID int, limit int) (messages []MessageInfo, nextOffsetID int, hasMore bool, err error) {
	api := client.API()
	nextOffsetID = offsetID

	err = withTakeout(ctx, api, chat.IsLeft, func(takeoutID int64) error {
		rawMessages, e := searchMessagesWithOffset(ctx, api, chat.InputPeer, selfID, takeoutID, offsetID, limit)
		if e != nil {
			return fmt.Errorf("fetching messages: %w", e)
		}
		messages, nextOffsetID = toMessageInfos(rawMessages, offsetID)
		// More pages exist when the server returned a full raw page; decide on the
		// raw count, not the filtered one, or service messages would truncate history.
		hasMore = len(rawMessages) == limit
		return nil
	})
	if err != nil {
		return nil, 0, false, err
	}
	return messages, nextOffsetID, hasMore, nil
}

// toMessageInfos converts a raw search page to MessageInfo entries, skipping
// service/empty items, and returns the pagination cursor advanced over every
// raw message (including the skipped ones, so a service-message tail cannot
// stall pagination).
func toMessageInfos(rawMsgs []tg.MessageClass, offsetID int) ([]MessageInfo, int) {
	nextOffset := offsetID
	var infos []MessageInfo
	for _, m := range rawMsgs {
		nextOffset = m.GetID() // advance by every raw message, including filtered ones
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		text := msg.Message
		if text == "" {
			text = "[media]"
		}
		infos = append(infos, MessageInfo{
			ID:   msg.ID,
			Date: msg.Date,
			Text: text,
		})
	}
	return infos, nextOffset
}

// collectDeletableIDs returns the IDs from a raw search page that are safe to
// delete, and the pagination cursor advanced over every raw message. Safety
// check: only messages we can positively confirm are ours are collected. The
// server search already filters by FromID=self, but if a message carries no
// confirmable PeerUser author, it is skipped rather than risk deleting someone
// else's message.
func collectDeletableIDs(rawMsgs []tg.MessageClass, selfID int64, offsetID int) (ids []int, nextOffset int) {
	nextOffset = offsetID
	for _, m := range rawMsgs {
		nextOffset = m.GetID() // advance by every raw message, incl. service/empty ones
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		fromID, ok := msg.GetFromID()
		if !ok {
			continue
		}
		peer, ok := fromID.(*tg.PeerUser)
		if !ok || peer.UserID != selfID {
			continue
		}
		ids = append(ids, msg.ID)
	}
	return ids, nextOffset
}

// hasOwnReaction reports whether the message carries a reaction chosen by the
// current user (Telegram marks own reactions with ChosenOrder).
func hasOwnReaction(msg *tg.Message) bool {
	for _, r := range msg.Reactions.Results {
		if _, ok := r.GetChosenOrder(); ok {
			return true
		}
	}
	return false
}

type DeleteProgress struct {
	Deleted int
	Total   int
	Done    bool
	Err     error
}

// sendProgress sends v on ch unless ctx is cancelled first. Returns false if
// the send was abandoned because ctx was cancelled (e.g. the TUI quit mid-op),
// signalling the caller to stop so its deferred close(ch) can run. Without this,
// a background sender blocks forever on a full channel once the consumer stops
// reading, leaking the goroutine across account switches.
func sendProgress[T any](ctx context.Context, ch chan<- T, v T) bool {
	select {
	case ch <- v:
		return true
	case <-ctx.Done():
		return false
	}
}

// withTakeout runs fn inside an MTProto takeout session when one is needed
// (left chats), passing the session id (0 when not needed). Afterwards it makes
// a best-effort attempt to finish the session with Success = (fn returned nil).
// If ctx is already cancelled (quit/account switch mid-operation), the finish
// fails fast rather than blocking a tearing-down client, and the session is
// left to expire server-side.
func withTakeout(ctx context.Context, api *tg.Client, needed bool, fn func(takeoutID int64) error) error {
	if !needed {
		return fn(0)
	}
	takeout, err := api.AccountInitTakeoutSession(ctx, &tg.AccountInitTakeoutSessionRequest{
		MessageMegagroups: true,
		MessageChannels:   true,
	})
	if err != nil {
		return fmt.Errorf("init takeout: %w", err)
	}
	runErr := fn(takeout.ID)
	// Best-effort finish on every path. On cancellation (quit/account switch)
	// this fails fast rather than blocking a tearing-down client; the session
	// then auto-expires server-side.
	api.AccountFinishTakeoutSession(ctx, &tg.AccountFinishTakeoutSessionRequest{Success: runErr == nil})
	return runErr
}

func DeleteMyMessages(ctx context.Context, client *telegram.Client, chat ChatInfo, selfID int64, progress chan<- DeleteProgress) {
	defer close(progress)
	api := client.API()

	started := false
	err := withTakeout(ctx, api, chat.IsLeft, func(takeoutID int64) error {
		started = true
		// Phase 1: collect all message IDs by paginating through search with advancing offsetID.
		// Must complete BEFORE any deletion — otherwise the search index lag after batch-delete
		// causes the loop to exit early (server returns empty or stale results).
		var ids []int
		offsetID := 0
		for {
			rawMsgs, err := searchMessagesWithOffset(ctx, api, chat.InputPeer, selfID, takeoutID, offsetID, 100)
			if err != nil {
				sendProgress(ctx, progress, DeleteProgress{Err: err})
				return err
			}
			if len(rawMsgs) == 0 {
				break
			}

			pageIDs, nextOffset := collectDeletableIDs(rawMsgs, selfID, offsetID)
			ids = append(ids, pageIDs...)

			if !sendProgress(ctx, progress, DeleteProgress{Deleted: 0, Total: len(ids)}) {
				return ctx.Err()
			}

			if len(rawMsgs) < 100 {
				break
			}
			if nextOffset == offsetID {
				break // no progress, avoid infinite loop
			}
			offsetID = nextOffset
			time.Sleep(200 * time.Millisecond)
		}

		total := len(ids)
		if total == 0 {
			sendProgress(ctx, progress, DeleteProgress{Done: true})
			return nil
		}

		// Phase 2: delete collected IDs in chunks of 100.
		deleted := 0
		i := 0
		for i < total {
			end := min(i+100, total)
			chunk := ids[i:end]

			var err error
			switch peer := chat.InputPeer.(type) {
			case *tg.InputPeerChannel:
				deleteReq := &tg.ChannelsDeleteMessagesRequest{
					Channel: &tg.InputChannel{ChannelID: peer.ChannelID, AccessHash: peer.AccessHash},
					ID:      chunk,
				}
				if takeoutID != 0 {
					var result tg.MessagesAffectedMessages
					err = api.Invoker().Invoke(ctx, &tg.InvokeWithTakeoutRequest{
						TakeoutID: takeoutID,
						Query:     deleteReq,
					}, &result)
				} else {
					_, err = api.ChannelsDeleteMessages(ctx, deleteReq)
				}
			default:
				_, err = api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{
					Revoke: true,
					ID:     chunk,
				})
			}
			if err != nil {
				if wait, ok := tgerr.AsFloodWait(err); ok {
					if !sleepCtx(ctx, wait+time.Second) {
						return ctx.Err() // ctx cancelled
					}
					continue // retry same chunk
				}
				if tgerr.Is(err, "CHANNEL_PRIVATE") {
					sendProgress(ctx, progress, DeleteProgress{Deleted: deleted, Total: total, Err: fmt.Errorf("channel is private or deleted — messages can be viewed but not deleted")})
					return err
				}
				sendProgress(ctx, progress, DeleteProgress{Deleted: deleted, Total: total, Err: err})
				return err
			}

			deleted += len(chunk)
			if !sendProgress(ctx, progress, DeleteProgress{Deleted: deleted, Total: total}) {
				return ctx.Err()
			}
			i = end
			time.Sleep(300 * time.Millisecond)
		}

		sendProgress(ctx, progress, DeleteProgress{Deleted: deleted, Total: total, Done: true})
		return nil
	})
	if err != nil && !started {
		// withTakeout failed before the body ran (takeout init error); the body
		// reports its own errors, so only surface the init failure here.
		sendProgress(ctx, progress, DeleteProgress{Err: err})
	}
}

// sleepCtx waits for d, returning false if ctx is cancelled first. Used so a
// long FLOOD_WAIT back-off does not hang a background worker for minutes after
// the operation has already been cancelled (TUI quit / account switch).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func invokeWithRetry(ctx context.Context, api *tg.Client, takeoutID int64, req tg.MessagesSearchRequest, result *tg.MessagesMessagesBox) error {
	for {
		var err error
		if takeoutID != 0 {
			err = api.Invoker().Invoke(ctx, &tg.InvokeWithTakeoutRequest{
				TakeoutID: takeoutID,
				Query:     &req,
			}, result)
		} else {
			res, e := api.MessagesSearch(ctx, &req)
			if e != nil {
				err = e
			} else {
				result.Messages = res
				return nil
			}
		}
		if err != nil {
			if wait, ok := tgerr.AsFloodWait(err); ok {
				if !sleepCtx(ctx, wait+time.Second) {
					return ctx.Err()
				}
				continue
			}
			return err
		}
		return nil
	}
}

// retryFlood runs fn, waiting out FLOOD_WAIT errors and retrying until it
// succeeds or returns a non-flood error. Used for API calls that don't go
// through invokeWithRetry (which is specific to MessagesSearchRequest). The
// back-off is interrupted if ctx is cancelled.
func retryFlood(ctx context.Context, fn func() error) error {
	for {
		err := fn()
		if wait, ok := tgerr.AsFloodWait(err); ok {
			if !sleepCtx(ctx, wait+time.Second) {
				return ctx.Err()
			}
			continue
		}
		return err
	}
}

func searchMessagesWithOffset(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, selfID int64, takeoutID int64, offsetID int, limit int) ([]tg.MessageClass, error) {
	req := tg.MessagesSearchRequest{
		Peer:     peer,
		Q:        "",
		FromID:   &tg.InputPeerUser{UserID: selfID},
		Filter:   &tg.InputMessagesFilterEmpty{},
		OffsetID: offsetID,
		Limit:    limit,
	}

	var result tg.MessagesMessagesBox
	if err := invokeWithRetry(ctx, api, takeoutID, req, &result); err != nil {
		return nil, err
	}

	switch r := result.Messages.(type) {
	case *tg.MessagesMessages:
		return r.Messages, nil
	case *tg.MessagesMessagesSlice:
		return r.Messages, nil
	case *tg.MessagesChannelMessages:
		return r.Messages, nil
	}
	return nil, nil
}

func countMessages(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, selfID int64, takeoutID int64) (int, error) {
	req := tg.MessagesSearchRequest{
		Peer:   peer,
		Q:      "",
		FromID: &tg.InputPeerUser{UserID: selfID},
		Filter: &tg.InputMessagesFilterEmpty{},
		Limit:  1,
	}

	var result tg.MessagesMessagesBox
	if err := invokeWithRetry(ctx, api, takeoutID, req, &result); err != nil {
		return 0, err
	}

	switch r := result.Messages.(type) {
	case *tg.MessagesMessages:
		return len(r.Messages), nil
	case *tg.MessagesMessagesSlice:
		return r.Count, nil
	case *tg.MessagesChannelMessages:
		return r.Count, nil
	}
	return 0, nil
}

type ReactionProgress struct {
	Checked int
	Removed int
	Done    bool
	Err     error
}

func RemoveMyReactions(ctx context.Context, client *telegram.Client, chat ChatInfo, progress chan<- ReactionProgress) {
	defer close(progress)
	api := client.API()

	checked := 0
	removed := 0
	offsetID := 0

	for {
		var historyResult tg.MessagesMessagesClass
		err := retryFlood(ctx, func() error {
			var e error
			historyResult, e = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:     chat.InputPeer,
				OffsetID: offsetID,
				Limit:    100,
			})
			return e
		})
		if err != nil {
			sendProgress(ctx, progress, ReactionProgress{Checked: checked, Removed: removed, Err: err})
			return
		}

		var rawMsgs []tg.MessageClass
		switch r := historyResult.(type) {
		case *tg.MessagesMessages:
			rawMsgs = r.Messages
		case *tg.MessagesMessagesSlice:
			rawMsgs = r.Messages
		case *tg.MessagesChannelMessages:
			rawMsgs = r.Messages
		}

		if len(rawMsgs) == 0 {
			break
		}

		prevOffset := offsetID
		for _, m := range rawMsgs {
			offsetID = m.GetID() // advance by every raw message, incl. service/empty ones
			msg, ok := m.(*tg.Message)
			if !ok {
				continue
			}
			checked++

			if hasOwnReaction(msg) {
				err := retryFlood(ctx, func() error {
					_, e := api.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
						Peer:     chat.InputPeer,
						MsgID:    msg.ID,
						Reaction: []tg.ReactionClass{},
					})
					return e
				})
				if err != nil {
					sendProgress(ctx, progress, ReactionProgress{Checked: checked, Removed: removed, Err: err})
					return
				}
				removed++
				time.Sleep(300 * time.Millisecond)
			}
		}
		if offsetID == prevOffset {
			break // no progress, avoid infinite loop
		}

		if !sendProgress(ctx, progress, ReactionProgress{Checked: checked, Removed: removed}) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	sendProgress(ctx, progress, ReactionProgress{Checked: checked, Removed: removed, Done: true})
}

func LeaveChat(ctx context.Context, client *telegram.Client, chat ChatInfo, selfID int64) error {
	api := client.API()

	switch peer := chat.InputPeer.(type) {
	case *tg.InputPeerChannel:
		_, err := api.ChannelsLeaveChannel(ctx, &tg.InputChannel{
			ChannelID:  peer.ChannelID,
			AccessHash: peer.AccessHash,
		})
		if err != nil {
			return fmt.Errorf("leave channel: %w", err)
		}
	case *tg.InputPeerChat:
		_, err := api.MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{
			ChatID: peer.ChatID,
			UserID: &tg.InputUser{UserID: selfID},
		})
		if err != nil {
			return fmt.Errorf("leave chat: %w", err)
		}
	}
	return nil
}

type MessageCountProgress struct {
	ChatIndex int
	Count     int
	Total     int
	Err       error
}

func CountAllMyMessages(ctx context.Context, client *telegram.Client, chats []ChatInfo, selfID int64, progress chan<- MessageCountProgress) {
	defer close(progress)
	for i, chat := range chats {
		count, err := CountMyMessages(ctx, client, chat.InputPeer, selfID)
		if !sendProgress(ctx, progress, MessageCountProgress{
			ChatIndex: i,
			Count:     count,
			Total:     len(chats),
			Err:       err,
		}) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func sanitizeFilename(name string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	return r.Replace(name)
}

type ExportProgress struct {
	Exported int
	Total    int
	Done     bool
	Err      error
	FilePath string
}

func ExportMyMessages(ctx context.Context, client *telegram.Client, chat ChatInfo, selfID int64, phone string, progress chan<- ExportProgress) {
	defer close(progress)
	api := client.API()

	started := false
	err := withTakeout(ctx, api, chat.IsLeft, func(takeoutID int64) error {
		started = true
		total, err := countMessages(ctx, api, chat.InputPeer, selfID, takeoutID)
		if err != nil {
			sendProgress(ctx, progress, ExportProgress{Err: err})
			return err
		}
		if total == 0 {
			sendProgress(ctx, progress, ExportProgress{Done: true})
			return nil
		}

		filename := sanitizeFilename(chat.Title) + " (" + phone + ").txt"
		f, err := os.Create(filename)
		if err != nil {
			sendProgress(ctx, progress, ExportProgress{Err: fmt.Errorf("create file: %w", err)})
			return err
		}
		success := false
		defer func() {
			f.Close() // idempotent; the checked Close is the explicit one below
			if !success {
				os.Remove(filename) // don't leave a partial/corrupt export behind
			}
		}()

		if _, err := fmt.Fprintf(f, "Export: %s\nAccount: %s\nDate: %s\nMessages: %d\n\n", chat.Title, phone, time.Now().Format("2006-01-02 15:04"), total); err != nil {
			sendProgress(ctx, progress, ExportProgress{Err: fmt.Errorf("write file: %w", err)})
			return err
		}

		exported := 0
		offsetID := 0
		for {
			rawMsgs, err := searchMessagesWithOffset(ctx, api, chat.InputPeer, selfID, takeoutID, offsetID, 100)
			if err != nil {
				sendProgress(ctx, progress, ExportProgress{Exported: exported, Total: total, Err: err})
				return err
			}
			if len(rawMsgs) == 0 {
				break
			}
			newOffsetID := offsetID
			for _, m := range rawMsgs {
				newOffsetID = m.GetID() // advance by every raw message, incl. service/empty ones
				msg, ok := m.(*tg.Message)
				if !ok {
					continue
				}
				text := msg.Message
				if text == "" {
					text = "[media]"
				}
				t := time.Unix(int64(msg.Date), 0)
				if _, err := fmt.Fprintf(f, "[%s] %s\n", t.Format("2006-01-02 15:04"), text); err != nil {
					sendProgress(ctx, progress, ExportProgress{Exported: exported, Total: total, Err: fmt.Errorf("write file: %w", err)})
					return err
				}
				exported++
			}
			if newOffsetID == offsetID {
				break // no progress, avoid infinite loop
			}
			offsetID = newOffsetID
			if !sendProgress(ctx, progress, ExportProgress{Exported: exported, Total: total}) {
				return ctx.Err()
			}
			time.Sleep(200 * time.Millisecond)
		}

		if err := f.Close(); err != nil {
			sendProgress(ctx, progress, ExportProgress{Exported: exported, Total: total, Err: fmt.Errorf("close file: %w", err)})
			return err
		}
		success = true
		sendProgress(ctx, progress, ExportProgress{Exported: exported, Total: total, Done: true, FilePath: filename})
		return nil
	})
	if err != nil && !started {
		sendProgress(ctx, progress, ExportProgress{Err: err}) // takeout init failed before the body ran
	}
}

type LeftChannelsProgress struct {
	Chats   []ChatInfo
	Found   int
	Counted int // how many of Found have had their message count resolved
	Done    bool
	Err     error
}

// snapshotChats returns a copy of chats safe to hand to another goroutine:
// GetLeftChannels keeps mutating its slice (filling MyMessages) after each
// progress send, so consumers must never share its backing array.
func snapshotChats(chats []ChatInfo) []ChatInfo {
	return append([]ChatInfo(nil), chats...)
}

func GetLeftChannels(ctx context.Context, client *telegram.Client, selfID int64, progress chan<- LeftChannelsProgress) {
	defer close(progress)
	api := client.API()

	started := false
	err := withTakeout(ctx, api, true, func(takeoutID int64) error {
		started = true
		invoker := api.Invoker()
		var allChats []ChatInfo
		offset := 0

		for {
			var result tg.MessagesChatsBox
			err := invoker.Invoke(ctx, &tg.InvokeWithTakeoutRequest{
				TakeoutID: takeoutID,
				Query:     &tg.ChannelsGetLeftChannelsRequest{Offset: offset},
			}, &result)
			if err != nil {
				sendProgress(ctx, progress, LeftChannelsProgress{Chats: allChats, Found: len(allChats), Err: fmt.Errorf("get left channels: %w", err)})
				return err
			}

			chats := result.Chats
			var chatList []tg.ChatClass
			switch c := chats.(type) {
			case *tg.MessagesChats:
				chatList = c.Chats
			case *tg.MessagesChatsSlice:
				chatList = c.Chats
			}

			if len(chatList) == 0 {
				break
			}

			for _, c := range chatList {
				ch, ok := c.(*tg.Channel)
				if !ok {
					continue
				}
				ct := ChatTypeChannel
				if ch.Megagroup {
					ct = ChatTypeSupergroup
				}
				username := ""
				if u, ok := ch.GetUsername(); ok {
					username = u
				}
				allChats = append(allChats, ChatInfo{
					Title:      ch.Title,
					ID:         ch.ID,
					Type:       ct,
					Username:   username,
					InputPeer:  &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash},
					MyMessages: -1,
					IsLeft:     true,
				})
			}

			offset += len(chatList)
			if !sendProgress(ctx, progress, LeftChannelsProgress{Chats: snapshotChats(allChats), Found: len(allChats)}) {
				return ctx.Err()
			}
			time.Sleep(200 * time.Millisecond)
		}

		// Count messages in left channels via takeout. countMessages routes through
		// invokeWithRetry, so transient FLOOD_WAIT is waited out rather than skipped.
		for i := range allChats {
			count, err := countMessages(ctx, api, allChats[i].InputPeer, selfID, takeoutID)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err() // cancelled: stop and finish takeout with Success=false
				}
				// Leave MyMessages at -1 for channels where counting genuinely
				// fails (e.g. CHANNEL_PRIVATE); still advance the progress counter.
			} else {
				allChats[i].MyMessages = count
			}
			if !sendProgress(ctx, progress, LeftChannelsProgress{Chats: snapshotChats(allChats), Found: len(allChats), Counted: i + 1}) {
				return ctx.Err()
			}
			time.Sleep(200 * time.Millisecond)
		}

		sendProgress(ctx, progress, LeftChannelsProgress{Chats: allChats, Found: len(allChats), Done: true})
		return nil
	})
	if err != nil && !started {
		sendProgress(ctx, progress, LeftChannelsProgress{Err: err}) // takeout init failed before the body ran
	}
}
