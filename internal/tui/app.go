// Package tui implements the terminal interface using Bubble Tea.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	tg "github.com/anik1ng/tgmsgcleaner/internal/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/skip2/go-qrcode"
)

// ExitAction tells main.go what to do after the TUI exits.
type ExitAction string

const (
	ExitQuit   ExitAction = ""       // leave the app
	ExitAdd    ExitAction = "add"    // ask for a phone number and add an account
	ExitSwitch ExitAction = "switch" // return to the account list
	ExitDelete ExitAction = "delete" // delete the account named in Selected
)

// Sign-in method on the auth screen. Nothing is requested from Telegram until
// the user picks one, so reopening the app does not burn login codes.
const (
	authModeChoose = iota
	authModeCode
	authModeQR
	authModeLoading // signed in; verifying the account and loading its chats
)

// Version is the application version shown in the UI. Local builds show
// "dev"; release builds override it from the git tag via
// -ldflags "-X github.com/anik1ng/tgmsgcleaner/internal/tui.Version=vX.Y.Z"
// (see .goreleaser.yaml).
var Version = "dev"

func appTitle() string {
	return "🧹 tgmsgcleaner " + Version
}

const (
	screenPicker = iota
	screenAuth
	screenPassword
	screenGroupList
	screenActions
	screenMessages
	screenConfirm
	screenDeleting
)

const (
	filterGroups      = 1
	filterSupergroups = 2
	filterChannels    = 3
	filterLeftWithMsg = 4
	filterLeftAll     = 5
)

var filterNames = []string{"All", "Groups", "Supergroups", "Channels", "Left+msg", "Left"}

var (
	tagGroup      = lipgloss.NewStyle().Foreground(lipgloss.Color("32")).Render("[GRP]")
	tagSupergroup = lipgloss.NewStyle().Foreground(lipgloss.Color("34")).Render("[SUP]")
	tagChannel    = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Render("[CHN]")
	tagLeft       = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("[LEFT]")
)

type App struct {
	client      *tg.Client
	ctx         context.Context
	phone       string
	sessionPath string
	selfID      int64
	codeHash    string
	codeVia     string // where Telegram sent the login code
	codeSending bool   // a login code request is in flight
	codeGen     int    // drops the result of an abandoned code request
	authErr     string
	authMode    int

	qrText   string             // rendered QR code for the current login token
	qrCancel context.CancelFunc // stops the running QR login
	qrGen    int                // drops messages from a cancelled QR login
	qrSignIn bool               // the sign-in in progress used a QR code, so the account must be verified

	termWidth, termHeight int // 0 until the first WindowSizeMsg

	screen    int
	authInput textinput.Model
	passInput textinput.Model
	list      list.Model
	progress  progress.Model

	allChats        []tg.ChatInfo
	filter          int
	loadingMessages bool
	loadedCount     int
	loadTotal       int
	countErrors     int    // number of chats whose message count failed
	groupListErr    string // error shown on the group list screen (e.g. left-channels load)
	leftLoaded      bool
	loadingLeft     bool
	leftFound       int
	leftCounted     int  // channels whose message count has been resolved so far
	leftShown       bool // left channels have appeared in the list at least once

	selectedChat *tg.ChatInfo
	actionList   list.Model
	msgList      list.Model
	messages     []tg.MessageInfo
	nextOffsetID int
	hasMore      bool
	loadingMore  bool
	msgFetchGen  int    // bumped on each view session; stale in-flight fetches are discarded
	msgErr       string // error shown on the messages screen when a fetch fails

	confirmText    string
	confirmAction  string
	deletingStatus string
	deletingPct    float64
	deletingDone   bool
	deletingErr    string

	ExitAction ExitAction // read by main.go after the TUI exits

	// Picker mode (no client)
	pickerList    list.Model
	pickerConfirm string // phone awaiting delete confirmation
	Selected      string // selected phone from picker — read by main.go
}

type accountItem struct {
	phone string
}

func (a accountItem) Title() string       { return a.phone }
func (a accountItem) Description() string { return "" }
func (a accountItem) FilterValue() string { return a.phone }

type Group struct {
	id         int64
	title      string
	desc       string
	chatType   tg.ChatType
	isLeft     bool
	myMessages int
}

type codeSentMsg struct {
	codeHash, codeVia string
	err               error
	gen               int
}

// authFailedMsg reports a sign-in that was undone; the user has to start over.
type authFailedMsg struct{ err error }
type qrMsg struct {
	url  string
	done bool
	err  error
	gen  int
	next tea.Cmd
}
type authDoneMsg struct{}
type authErrMsg struct{ err error }
type needPasswordMsg struct{}
type authRestartMsg struct{}
type chatsLoadedMsg struct {
	chats  []tg.ChatInfo
	selfID int64
}
type msgCountMsg struct {
	chatIndex int
	count     int
	total     int
	err       error
	nextCmd   tea.Cmd
}
type msgCountDoneMsg struct{}
type messagesLoadedMsg struct {
	messages     []tg.MessageInfo
	nextOffsetID int
	hasMore      bool
	gen          int
}
type messagesErrMsg struct {
	err error
	gen int
}

type deleteProgressMsg struct {
	deleted int
	total   int
	done    bool
	err     error
	nextCmd tea.Cmd
}

type reactionProgressMsg struct {
	checked int
	removed int
	done    bool
	err     error
	nextCmd tea.Cmd
}

type exportProgressMsg struct {
	exported int
	total    int
	done     bool
	err      error
	filePath string
	nextCmd  tea.Cmd
}

type leftChannelsMsg struct {
	chats   []tg.ChatInfo
	found   int
	counted int
	done    bool
	err     error
	nextCmd tea.Cmd
}

type leftGroupMsg struct{}

type ActionItem struct {
	title string
	key   string
}

func (a ActionItem) Title() string       { return a.title }
func (a ActionItem) Description() string { return "" }
func (a ActionItem) FilterValue() string { return a.title }

type MessageItem struct {
	date string
	text string
}

func (m MessageItem) Title() string       { return m.date }
func (m MessageItem) Description() string { return m.text }
func (m MessageItem) FilterValue() string { return m.text }

func NewApp(client *tg.Client, ctx context.Context, phone string, sessionPath string, isAuthorized bool) App {
	ti := textinput.New()
	ti.Placeholder = "Enter auth code..."
	ti.SetWidth(40)
	ti.Focus()

	pi := textinput.New()
	pi.Placeholder = "Enter 2FA password..."
	pi.SetWidth(40)
	pi.EchoMode = textinput.EchoPassword

	addAccountKey := key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add account"))
	switchAccountKey := key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "switch account"))
	leftChannelsKey := key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "find left channels"))

	l := list.New([]list.Item{}, list.NewDefaultDelegate(), 80, 24)
	l.Title = "All Chats"
	l.KeyMap.Quit.SetKeys("q")
	// Free h/l from page navigation: 'l' triggers the left-channels load on
	// this screen, and a leftover vim-style page-flip on repeated presses
	// makes the list jump around. Arrows and pgup/pgdn still page.
	l.KeyMap.NextPage.SetKeys("right", "pgdown", "f", "d")
	l.KeyMap.NextPage.SetHelp("→/pgdn", "next page")
	l.KeyMap.PrevPage.SetKeys("left", "pgup", "b", "u")
	l.KeyMap.PrevPage.SetHelp("←/pgup", "prev page")
	l.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{addAccountKey, switchAccountKey, leftChannelsKey}
	}
	l.AdditionalFullHelpKeys = func() []key.Binding {
		return []key.Binding{addAccountKey, switchAccountKey, leftChannelsKey}
	}

	actionDelegate := list.NewDefaultDelegate()
	actionDelegate.ShowDescription = false
	al := list.New([]list.Item{}, actionDelegate, 80, 24)
	al.SetFilteringEnabled(false)

	ml := list.New([]list.Item{}, list.NewDefaultDelegate(), 80, 24)
	ml.Title = "Messages"

	p := progress.New()

	screen := screenAuth
	if isAuthorized {
		screen = screenGroupList
	}

	return App{
		client:      client,
		ctx:         ctx,
		phone:       phone,
		sessionPath: sessionPath,
		screen:      screen,
		authInput:   ti,
		passInput:   pi,
		list:        l,
		actionList:  al,
		msgList:     ml,
		progress:    p,
	}
}

func NewPickerApp(accounts []string) App {
	items := make([]list.Item, len(accounts))
	for i, phone := range accounts {
		items[i] = accountItem{phone: phone}
	}

	delegate := list.NewDefaultDelegate()
	delegate.ShowDescription = false
	l := list.New(items, delegate, 40, 14)
	l.Title = "Select account"
	l.SetFilteringEnabled(false)
	l.KeyMap.Quit.SetKeys("q")
	// 'd' deletes an account here, so drop it from page navigation.
	l.KeyMap.NextPage.SetKeys("right", "l", "pgdown", "f")
	addKey := key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add account"))
	deleteKey := key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete account"))
	l.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{addKey, deleteKey}
	}
	l.AdditionalFullHelpKeys = func() []key.Binding {
		return []key.Binding{addKey, deleteKey}
	}

	return App{
		screen:     screenPicker,
		pickerList: l,
	}
}

func (a App) Init() tea.Cmd {
	if a.screen == screenGroupList {
		return a.loadChatsCmd()
	}
	return nil
}

func (a *App) sendCodeCmd(gen int) tea.Cmd {
	return func() tea.Msg {
		sent, err := tg.SendCode(a.ctx, a.client, a.phone, a.sessionPath)
		return codeSentMsg{codeHash: sent.Hash, codeVia: sent.Delivery, err: err, gen: gen}
	}
}

// requestCode asks Telegram for a new login code, abandoning any earlier
// request.
func (a *App) requestCode() tea.Cmd {
	a.codeGen++
	a.codeSending = true
	a.codeHash = ""
	a.codeVia = ""
	a.authErr = ""
	a.authInput.SetValue("")
	return a.sendCodeCmd(a.codeGen)
}

// signedIn shows the loading screen after a successful sign-in and starts
// loading the account.
func (a *App) signedIn() tea.Cmd {
	a.screen = screenAuth
	a.authMode = authModeLoading
	a.authErr = ""
	a.qrText = ""
	return a.finishLoginCmd()
}

// finishLoginCmd loads the chats of a freshly signed-in account. After a QR
// login it first confirms that the code was scanned by the account this
// session is saved under.
func (a *App) finishLoginCmd() tea.Cmd {
	verify := a.qrSignIn
	load := a.loadChatsCmd()
	return func() tea.Msg {
		if verify {
			err := tg.VerifyAccount(a.ctx, a.client, a.phone)
			if errors.Is(err, tg.ErrAccountMismatch) {
				return authFailedMsg{err: err}
			}
			if err != nil {
				return authErrMsg{err: err}
			}
		}
		return load()
	}
}

// resetAuth returns to the sign-in choice with a clean slate and shows why.
func (a *App) resetAuth(reason string) {
	a.stopQR()
	a.qrSignIn = false
	a.qrText = ""
	a.codeGen++ // drop the result of a code request still in flight
	a.codeSending = false
	a.codeHash = ""
	a.codeVia = ""
	a.screen = screenAuth
	a.authMode = authModeChoose
	a.authInput.SetValue("")
	a.authInput.Focus()
	a.passInput.SetValue("")
	a.authErr = reason
}

// startQR begins a QR login, replacing any QR login already running.
func (a *App) startQR() tea.Cmd {
	a.stopQR()
	ctx, cancel := context.WithCancel(a.ctx)
	a.qrCancel = cancel
	a.qrSignIn = true
	a.authMode = authModeQR
	a.qrText = ""
	a.authErr = ""
	ch := make(chan tg.QRProgress, 1)
	go tg.QRLogin(ctx, a.client, ch)
	return waitForQR(ch, a.qrGen)
}

// stopQR cancels the running QR login and invalidates its pending messages.
func (a *App) stopQR() {
	if a.qrCancel != nil {
		a.qrCancel()
		a.qrCancel = nil
	}
	a.qrGen++
}

func waitForQR(ch <-chan tg.QRProgress, gen int) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			// Closed without a final result: the login was cancelled.
			return qrMsg{done: true, err: context.Canceled, gen: gen}
		}
		return qrMsg{url: p.URL, done: p.Done, err: p.Err, gen: gen, next: waitForQR(ch, gen)}
	}
}

// qrStyle forces white on black so the code scans on any terminal theme:
// renderQR draws light modules with the foreground color and leaves dark ones
// as background.
var qrStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#000000"))

// renderQR draws a QR code with half-block characters.
func renderQR(url string) (string, error) {
	code, err := qrcode.New(url, qrcode.Low)
	if err != nil {
		return "", fmt.Errorf("render qr code: %w", err)
	}
	return qrStyle.Render(strings.TrimRight(code.ToSmallString(false), "\n")), nil
}

func (a *App) loadChatsCmd() tea.Cmd {
	return func() tea.Msg {
		selfID, err := tg.GetSelfID(a.ctx, a.client)
		if err != nil {
			return authErrMsg{err: err}
		}
		chats, err := tg.GetChats(a.ctx, a.client)
		if err != nil {
			return authErrMsg{err: err}
		}
		return chatsLoadedMsg{chats: chats, selfID: selfID}
	}
}

func waitForCount(ch <-chan tg.MessageCountProgress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return msgCountDoneMsg{}
		}
		return msgCountMsg{
			chatIndex: p.ChatIndex,
			count:     p.Count,
			total:     p.Total,
			err:       p.Err,
			nextCmd:   waitForCount(ch),
		}
	}
}

func (a *App) applyFilter() {
	var items []list.Item
	for _, c := range a.allChats {
		switch a.filter {
		case filterGroups:
			if c.Type != tg.ChatTypeGroup {
				continue
			}
		case filterSupergroups:
			if c.Type != tg.ChatTypeSupergroup {
				continue
			}
		case filterChannels:
			if c.Type != tg.ChatTypeChannel {
				continue
			}
		case filterLeftAll:
			if !c.IsLeft {
				continue
			}
		case filterLeftWithMsg:
			if !c.IsLeft || c.MyMessages <= 0 {
				continue
			}
		}
		items = append(items, Group{
			id:         c.ID,
			title:      c.Title,
			desc:       formatDescription(c),
			chatType:   c.Type,
			isLeft:     c.IsLeft,
			myMessages: c.MyMessages,
		})
	}
	a.list.SetItems(items)
	a.list.Title = filterNames[a.filter]
}

func formatDescription(c tg.ChatInfo) string {
	var parts []string
	if c.IsLeft {
		if c.Username != "" {
			parts = append(parts, "t.me/"+c.Username)
		} else {
			parts = append(parts, fmt.Sprintf("t.me/c/%d", c.ID))
		}
	}
	if c.MemberCount > 0 {
		parts = append(parts, fmt.Sprintf("%d members", c.MemberCount))
	}
	if !c.IsLeft && c.Username != "" {
		parts = append(parts, "@"+c.Username)
	}
	if c.MyMessages >= 0 {
		parts = append(parts, fmt.Sprintf("%d your msgs", c.MyMessages))
	}
	return strings.Join(parts, " · ")
}

var statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

func (a App) filterCount() int {
	if a.leftLoaded || a.leftShown {
		return 6 // All, Groups, Supergroups, Channels, Left+msg, Left
	}
	return 4 // All, Groups, Supergroups, Channels
}

func (a App) statusBar() string {
	totalMsgs := 0
	for _, c := range a.allChats {
		if c.MyMessages >= 0 {
			totalMsgs += c.MyMessages
		}
	}
	msgs := fmt.Sprintf("%d msgs", totalMsgs)
	if a.loadingMessages {
		msgs += "..." // counting still in progress; failures are shown separately on the list
	}
	return statusStyle.Render(fmt.Sprintf("  %s · %d groups · %s", a.phone, len(a.allChats), msgs))
}

func (a *App) openActions(chat tg.ChatInfo) {
	a.selectedChat = &chat
	a.screen = screenActions
	title := chat.Title
	if chat.MyMessages >= 0 {
		title += fmt.Sprintf(" (%d msgs)", chat.MyMessages)
	}
	a.actionList.Title = title
	items := []list.Item{
		ActionItem{title: "View my messages", key: "view"},
		ActionItem{title: "Export my messages", key: "export"},
		ActionItem{title: "Delete my messages", key: "delete_msgs"},
	}
	// Reactions and leaving require active membership and don't work via
	// takeout, so they make no sense for channels the user has left.
	if !chat.IsLeft {
		items = append(items,
			ActionItem{title: "Delete my reactions", key: "delete_reactions"},
			ActionItem{title: "Leave group", key: "leave"},
		)
	}
	a.actionList.SetItems(items)
}

func (a *App) fetchMessagesCmd(offsetID int, gen int) tea.Cmd {
	chat := a.selectedChat
	selfID := a.selfID
	return func() tea.Msg {
		msgs, nextOffset, hasMore, err := tg.FetchMyMessages(a.ctx, a.client, *chat, selfID, offsetID, 50)
		if err != nil {
			return messagesErrMsg{err: err, gen: gen}
		}
		return messagesLoadedMsg{
			messages:     msgs,
			nextOffsetID: nextOffset,
			hasMore:      hasMore,
			gen:          gen,
		}
	}
}

func waitForDelete(ch <-chan tg.DeleteProgress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return deleteProgressMsg{done: true}
		}
		return deleteProgressMsg{
			deleted: p.Deleted,
			total:   p.Total,
			done:    p.Done,
			err:     p.Err,
			nextCmd: waitForDelete(ch),
		}
	}
}

func waitForReactions(ch <-chan tg.ReactionProgress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return reactionProgressMsg{done: true}
		}
		return reactionProgressMsg{
			checked: p.Checked,
			removed: p.Removed,
			done:    p.Done,
			err:     p.Err,
			nextCmd: waitForReactions(ch),
		}
	}
}

func waitForExport(ch <-chan tg.ExportProgress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return exportProgressMsg{done: true}
		}
		return exportProgressMsg{
			exported: p.Exported,
			total:    p.Total,
			done:     p.Done,
			err:      p.Err,
			filePath: p.FilePath,
			nextCmd:  waitForExport(ch),
		}
	}
}

func waitForLeftChannels(ch <-chan tg.LeftChannelsProgress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return leftChannelsMsg{done: true}
		}
		return leftChannelsMsg{
			chats:   p.Chats,
			found:   p.Found,
			counted: p.Counted,
			done:    p.Done,
			err:     p.Err,
			nextCmd: waitForLeftChannels(ch),
		}
	}
}

func (a *App) buildMessageItems() []list.Item {
	items := make([]list.Item, 0, len(a.messages))
	for _, m := range a.messages {
		t := time.Unix(int64(m.Date), 0)
		items = append(items, MessageItem{
			date: t.Format("[2006-01-02 15:04]"),
			text: m.Text,
		})
	}
	if a.hasMore {
		items = append(items, MessageItem{
			date: "↓",
			text: "Load more messages... (Enter)",
		})
	}
	return items
}

func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		a.termWidth, a.termHeight = msg.Width, msg.Height
		if a.screen == screenPicker {
			a.pickerList.SetSize(msg.Width, msg.Height-4)
		} else {
			a.list.SetSize(msg.Width, msg.Height-6)
			a.actionList.SetSize(msg.Width, msg.Height-2)
			a.msgList.SetSize(msg.Width, msg.Height-2)
			a.progress.SetWidth(msg.Width - 10)
		}
		return a, nil

	case codeSentMsg:
		if msg.gen != a.codeGen {
			return a, nil // from an abandoned code request
		}
		a.codeSending = false
		onCodePath := a.screen == screenAuth && (a.authMode == authModeCode || a.authMode == authModeChoose)
		if msg.err != nil {
			// Report the failure only while the user is still on the code
			// path; they may have moved on to a QR login in the meantime.
			if onCodePath {
				a.authMode = authModeChoose
				a.authErr = msg.err.Error()
			}
			return a, nil
		}
		a.codeHash = msg.codeHash
		a.codeVia = msg.codeVia
		if onCodePath {
			a.authErr = ""
		}
		return a, nil

	case authRestartMsg:
		// Telegram reset the auth flow; the session was already cleared. Return
		// to the sign-in choice so the user picks a method again.
		a.resetAuth("Session was reset — sign in again")
		return a, nil

	case authFailedMsg:
		a.resetAuth(msg.err.Error())
		return a, nil

	case qrMsg:
		if msg.gen != a.qrGen {
			return a, nil // from a cancelled QR login
		}
		if !msg.done {
			text, err := renderQR(msg.url)
			if err != nil {
				a.authErr = err.Error()
			}
			a.qrText = text
			return a, msg.next
		}
		// The login is over either way, so the code on screen is dead.
		a.stopQR()
		a.qrText = ""
		switch {
		case msg.err == nil:
			cmd := a.signedIn()
			return a, cmd
		case errors.Is(msg.err, auth.ErrPasswordAuthNeeded):
			return a, func() tea.Msg { return needPasswordMsg{} }
		default:
			a.qrSignIn = false
			a.authMode = authModeChoose
			a.authErr = msg.err.Error()
			if errors.Is(msg.err, context.Canceled) {
				// Not the user's esc (that drops the message above): the
				// client itself stopped.
				a.authErr = "QR login was interrupted — try again"
			}
			return a, nil
		}

	case authDoneMsg:
		cmd := a.signedIn()
		return a, cmd

	case chatsLoadedMsg:
		a.allChats = msg.chats
		a.selfID = msg.selfID
		a.applyFilter()
		a.screen = screenGroupList
		a.loadingMessages = true
		a.loadedCount = 0
		a.loadTotal = len(a.allChats)
		a.countErrors = 0
		ch := make(chan tg.MessageCountProgress, 1)
		go tg.CountAllMyMessages(a.ctx, a.client, a.allChats, a.selfID, ch)
		return a, waitForCount(ch)

	case msgCountMsg:
		if msg.chatIndex < len(a.allChats) {
			if msg.err != nil {
				a.countErrors++
			} else {
				a.allChats[msg.chatIndex].MyMessages = msg.count
			}
		}
		a.loadedCount = msg.chatIndex + 1
		a.loadTotal = msg.total
		a.applyFilter()
		return a, msg.nextCmd

	case msgCountDoneMsg:
		a.loadingMessages = false
		a.applyFilter()
		return a, nil

	case messagesLoadedMsg:
		if msg.gen != a.msgFetchGen {
			return a, nil // stale fetch from a previous view session — discard
		}
		a.msgErr = ""
		a.messages = append(a.messages, msg.messages...)
		a.hasMore = msg.hasMore
		a.loadingMore = false
		a.nextOffsetID = msg.nextOffsetID
		a.msgList.SetItems(a.buildMessageItems())
		return a, nil

	case messagesErrMsg:
		if msg.gen != a.msgFetchGen {
			return a, nil // stale fetch from a previous view session — discard
		}
		a.msgErr = msg.err.Error()
		a.loadingMore = false
		return a, nil

	case deleteProgressMsg:
		if msg.err != nil {
			a.deletingErr = msg.err.Error()
			a.deletingDone = true
			return a, nil
		}
		if msg.total > 0 {
			a.deletingPct = float64(msg.deleted) / float64(msg.total)
		}
		a.deletingStatus = fmt.Sprintf("Deleting messages... %d/%d", msg.deleted, msg.total)
		if msg.done {
			a.deletingDone = true
			a.deletingStatus = fmt.Sprintf("Done! Deleted %d messages.", msg.deleted)
			if a.selectedChat != nil {
				a.selectedChat.MyMessages = 0
				for i := range a.allChats {
					// (ID, Type) is the unique key: raw chat.ID and channel.ID are
					// separate spaces, so match both or we may zero the wrong row.
					if a.allChats[i].ID == a.selectedChat.ID && a.allChats[i].Type == a.selectedChat.Type {
						a.allChats[i].MyMessages = 0
						break
					}
				}
				a.applyFilter()
			}
			return a, nil
		}
		return a, msg.nextCmd

	case reactionProgressMsg:
		if msg.err != nil {
			a.deletingErr = msg.err.Error()
			a.deletingDone = true
			return a, nil
		}
		a.deletingStatus = fmt.Sprintf("Removing reactions... checked %d, removed %d", msg.checked, msg.removed)
		a.deletingPct = 0
		if msg.done {
			a.deletingDone = true
			a.deletingStatus = fmt.Sprintf("Done! Removed %d reactions.", msg.removed)
			return a, nil
		}
		return a, msg.nextCmd

	case exportProgressMsg:
		if msg.err != nil {
			a.deletingErr = msg.err.Error()
			a.deletingDone = true
			return a, nil
		}
		if msg.total > 0 {
			a.deletingPct = float64(msg.exported) / float64(msg.total)
		}
		a.deletingStatus = fmt.Sprintf("Exporting messages... %d/%d", msg.exported, msg.total)
		if msg.done {
			a.deletingDone = true
			a.deletingStatus = fmt.Sprintf("Done! Exported %d messages to %s", msg.exported, msg.filePath)
			return a, nil
		}
		return a, msg.nextCmd

	case leftChannelsMsg:
		if msg.err != nil {
			a.loadingLeft = false
			a.groupListErr = msg.err.Error()
			return a, nil
		}
		a.leftFound = msg.found
		a.leftCounted = msg.counted
		if len(msg.chats) > 0 {
			// Replace the left-channel tail with the latest snapshot. IsLeft
			// marks exactly the chats owned by this loader, so stripping by it
			// keeps regular chats intact and never duplicates on updates.
			kept := a.allChats[:0:0]
			for _, c := range a.allChats {
				if !c.IsLeft {
					kept = append(kept, c)
				}
			}
			a.allChats = append(kept, msg.chats...)
			if !a.leftShown {
				// First results just arrived: jump to the view the user asked
				// for instead of leaving them on 'All' with a merged list.
				a.leftShown = true
				a.filter = filterLeftWithMsg
			}
			a.applyFilter()
		}
		if msg.done {
			a.loadingLeft = false
			a.leftLoaded = true
			a.applyFilter()
			return a, nil
		}
		return a, msg.nextCmd

	case leftGroupMsg:
		for i, c := range a.allChats {
			// Match on (ID, Type): raw chat.ID and channel.ID are separate spaces,
			// so the same number can be both a basic group and a channel.
			if a.selectedChat != nil && c.ID == a.selectedChat.ID && c.Type == a.selectedChat.Type {
				a.allChats = append(a.allChats[:i], a.allChats[i+1:]...)
				break
			}
		}
		a.applyFilter()
		a.selectedChat = nil
		a.screen = screenGroupList
		return a, nil

	case authErrMsg:
		a.authErr = msg.err.Error()
		return a, nil

	case needPasswordMsg:
		a.screen = screenPassword
		a.authErr = ""
		a.passInput.Focus()
		return a, nil

	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return a, tea.Quit
		}

		// While a search query is being typed the list owns the keyboard;
		// otherwise letters such as "a", "l" and "s" would fire their hotkeys.
		if a.screen == screenGroupList && a.list.FilterState() == list.Filtering {
			var cmd tea.Cmd
			a.list, cmd = a.list.Update(msg)
			return a, cmd
		}

		if a.screen == screenPicker {
			if a.pickerConfirm != "" {
				switch msg.String() {
				case "y":
					// main.go deletes the account and reopens the picker.
					a.Selected = a.pickerConfirm
					a.ExitAction = ExitDelete
					return a, tea.Quit
				case "esc", "n":
					a.pickerConfirm = ""
				}
				return a, nil
			}
			switch msg.String() {
			case "enter":
				if item, ok := a.pickerList.SelectedItem().(accountItem); ok {
					a.Selected = item.phone
				}
				return a, tea.Quit
			case "a":
				a.ExitAction = ExitAdd
				return a, tea.Quit
			case "d":
				if item, ok := a.pickerList.SelectedItem().(accountItem); ok {
					a.pickerConfirm = item.phone
				}
				return a, nil
			case "q":
				return a, tea.Quit
			}
		}

		if a.screen == screenAuth {
			switch a.authMode {
			case authModeChoose:
				switch msg.String() {
				case "c":
					a.authMode = authModeCode
					a.authErr = ""
					if a.codeHash != "" || a.codeSending {
						// A code is already sent or on its way; asking again
						// would burn another one.
						return a, nil
					}
					cmd := a.requestCode()
					return a, cmd
				case "r":
					// Not "return a, a.startQR()": startQR changes a, and Go
					// does not say whether a is copied before or after the call.
					cmd := a.startQR()
					return a, cmd
				case "q":
					return a, tea.Quit
				}
				return a, nil
			case authModeQR:
				if msg.String() == "esc" {
					a.stopQR()
					a.qrSignIn = false
					a.authMode = authModeChoose
					a.authErr = ""
				}
				return a, nil
			case authModeLoading:
				// Keys only matter once loading has failed.
				if a.authErr != "" {
					switch msg.String() {
					case "enter":
						a.authErr = ""
						return a, a.finishLoginCmd()
					case "q":
						return a, tea.Quit
					}
				}
				return a, nil
			case authModeCode:
				switch msg.String() {
				case "esc":
					// Keep the pending code so coming back does not request
					// another one.
					a.authMode = authModeChoose
					a.authErr = ""
					return a, nil
				case "ctrl+r":
					if a.codeSending {
						return a, nil
					}
					cmd := a.requestCode()
					return a, cmd
				}
			}
		}

		if msg.String() == "enter" && a.screen == screenAuth && a.codeHash != "" {
			code := a.authInput.Value()
			return a, func() tea.Msg {
				err := tg.SignIn(a.ctx, a.client, a.phone, code, a.codeHash, a.sessionPath)
				if err != nil {
					if errors.Is(err, auth.ErrPasswordAuthNeeded) {
						return needPasswordMsg{}
					}
					if errors.Is(err, tg.ErrAuthRestart) {
						return authRestartMsg{}
					}
					return authErrMsg{err: err}
				}
				return authDoneMsg{}
			}
		}

		if msg.String() == "enter" && a.screen == screenPassword {
			password := a.passInput.Value()
			return a, func() tea.Msg {
				err := tg.CheckPassword(a.ctx, a.client, password, a.sessionPath)
				if err != nil {
					if errors.Is(err, tg.ErrAuthRestart) {
						return authRestartMsg{}
					}
					return authErrMsg{err: err}
				}
				return authDoneMsg{}
			}
		}

		if a.screen == screenConfirm {
			if msg.String() == "y" {
				switch a.confirmAction {
				case "export":
					a.screen = screenDeleting
					a.deletingStatus = "Starting export..."
					a.deletingDone = false
					a.deletingErr = ""
					a.deletingPct = 0
					ch := make(chan tg.ExportProgress, 1)
					go tg.ExportMyMessages(a.ctx, a.client, *a.selectedChat, a.selfID, a.phone, ch)
					return a, waitForExport(ch)
				case "delete_msgs":
					a.screen = screenDeleting
					a.deletingStatus = "Starting deletion..."
					a.deletingDone = false
					a.deletingErr = ""
					a.deletingPct = 0
					ch := make(chan tg.DeleteProgress, 1)
					go tg.DeleteMyMessages(a.ctx, a.client, *a.selectedChat, a.selfID, ch)
					return a, waitForDelete(ch)
				case "delete_reactions":
					a.screen = screenDeleting
					a.deletingStatus = "Starting reaction removal..."
					a.deletingDone = false
					a.deletingErr = ""
					a.deletingPct = 0
					ch := make(chan tg.ReactionProgress, 1)
					go tg.RemoveMyReactions(a.ctx, a.client, *a.selectedChat, ch)
					return a, waitForReactions(ch)
				case "leave":
					return a, func() tea.Msg {
						err := tg.LeaveChat(a.ctx, a.client, *a.selectedChat, a.selfID)
						if err != nil {
							return authErrMsg{err: err}
						}
						return leftGroupMsg{}
					}
				}
			}
			if msg.String() == "esc" || msg.String() == "n" {
				a.screen = screenActions
				return a, nil
			}
			return a, nil
		}

		if a.screen == screenDeleting && a.deletingDone {
			if msg.String() == "esc" || msg.String() == "enter" {
				a.screen = screenActions
				a.deletingDone = false
				a.deletingErr = ""
				a.deletingStatus = ""
				a.deletingPct = 0
				return a, nil
			}
			return a, nil
		}

		if msg.String() == "enter" && a.screen == screenGroupList {
			if item, ok := a.list.SelectedItem().(Group); ok {
				if item.isLeft && item.myMessages <= 0 {
					return a, nil
				}
				for _, c := range a.allChats {
					// (ID, Type) is the unique key: raw MTProto chat.ID and
					// channel.ID are separate spaces, so the same number can be
					// both a basic group and a channel.
					if c.ID == item.id && c.Type == item.chatType {
						a.openActions(c)
						break
					}
				}
			}
			return a, nil
		}

		if msg.String() == "enter" && a.screen == screenActions {
			if item, ok := a.actionList.SelectedItem().(ActionItem); ok {
				switch item.key {
				case "view":
					a.screen = screenMessages
					a.messages = nil
					a.nextOffsetID = 0
					a.hasMore = true
					a.loadingMore = true
					a.msgErr = ""
					a.msgFetchGen++
					a.msgList.Title = a.selectedChat.Title + " — Messages"
					a.msgList.SetItems([]list.Item{})
					return a, a.fetchMessagesCmd(0, a.msgFetchGen)
				case "export":
					count := ""
					if a.selectedChat.MyMessages >= 0 {
						count = fmt.Sprintf(" (%d messages)", a.selectedChat.MyMessages)
					}
					a.confirmText = fmt.Sprintf("Export your messages from \"%s\"%s to file?\n\nPress [y] to confirm, [esc] to cancel", a.selectedChat.Title, count)
					a.confirmAction = "export"
					a.screen = screenConfirm
				case "delete_msgs":
					count := ""
					if a.selectedChat.MyMessages >= 0 {
						count = fmt.Sprintf(" (%d messages)", a.selectedChat.MyMessages)
					}
					a.confirmText = fmt.Sprintf("Delete all your messages in \"%s\"%s?\n\nPress [y] to confirm, [esc] to cancel", a.selectedChat.Title, count)
					a.confirmAction = "delete_msgs"
					a.screen = screenConfirm
				case "delete_reactions":
					a.confirmText = fmt.Sprintf("Remove all your reactions in \"%s\"?\n\nPress [y] to confirm, [esc] to cancel", a.selectedChat.Title)
					a.confirmAction = "delete_reactions"
					a.screen = screenConfirm
				case "leave":
					a.confirmText = fmt.Sprintf("Leave \"%s\"?\n\nPress [y] to confirm, [esc] to cancel", a.selectedChat.Title)
					a.confirmAction = "leave"
					a.screen = screenConfirm
				}
			}
			return a, nil
		}

		if msg.String() == "enter" && a.screen == screenMessages {
			if a.hasMore && !a.loadingMore {
				idx := a.msgList.Index()
				if idx == len(a.msgList.Items())-1 {
					a.loadingMore = true
					a.msgErr = ""
					return a, a.fetchMessagesCmd(a.nextOffsetID, a.msgFetchGen)
				}
			}
			return a, nil
		}

		if msg.String() == "esc" && a.screen == screenActions {
			a.screen = screenGroupList
			a.selectedChat = nil
			return a, nil
		}

		if msg.String() == "esc" && a.screen == screenMessages {
			a.screen = screenActions
			a.messages = nil
			a.msgErr = ""
			a.msgFetchGen++ // invalidate any in-flight fetch from this view session
			a.loadingMore = false
			return a, nil
		}

		if msg.String() == "l" && a.screen == screenGroupList {
			if !a.leftLoaded && !a.loadingLeft {
				a.loadingLeft = true
				a.leftFound = 0
				a.leftCounted = 0
				a.groupListErr = ""
				ch := make(chan tg.LeftChannelsProgress, 1)
				go tg.GetLeftChannels(a.ctx, a.client, a.selfID, ch)
				return a, waitForLeftChannels(ch)
			}
			// Already loading/loaded: swallow the key so it doesn't fall
			// through to the list widget (where 'l' would mean 'next page').
			return a, nil
		}

		if msg.String() == "a" && a.screen == screenGroupList {
			a.ExitAction = ExitAdd
			return a, tea.Quit
		}

		// Always available: the account list is also where accounts are added
		// and deleted, so it matters even with a single account.
		if msg.String() == "s" && a.screen == screenGroupList {
			a.ExitAction = ExitSwitch
			return a, tea.Quit
		}

		if msg.String() == "tab" && a.screen == screenGroupList {
			a.filter = (a.filter + 1) % a.filterCount()
			a.applyFilter()
			return a, nil
		}

		if msg.String() == "shift+tab" && a.screen == screenGroupList {
			fc := a.filterCount()
			a.filter = (a.filter - 1 + fc) % fc
			a.applyFilter()
			return a, nil
		}
	}

	var cmd tea.Cmd
	switch a.screen {
	case screenPicker:
		a.pickerList, cmd = a.pickerList.Update(msg)
	case screenAuth:
		a.authInput, cmd = a.authInput.Update(msg)
	case screenPassword:
		a.passInput, cmd = a.passInput.Update(msg)
	case screenGroupList:
		a.list, cmd = a.list.Update(msg)
	case screenActions:
		a.actionList, cmd = a.actionList.Update(msg)
	case screenMessages:
		a.msgList, cmd = a.msgList.Update(msg)
	}
	return a, cmd
}

// authView renders the sign-in screen for the current auth mode.
func (a App) authView() string {
	if a.authMode == authModeQR {
		return a.qrView()
	}
	s := appTitle() + "\n\n"
	switch a.authMode {
	case authModeChoose:
		s += fmt.Sprintf("Sign in as %s:\n\n  [c] send a login code\n  [r] scan a QR code with your phone\n  [q] quit\n", a.phone)
	case authModeCode:
		s += "Enter your Telegram auth code:\n\n" + a.authInput.View() + "\n"
		if a.codeHash == "" {
			s += "\nSending code...\n"
		} else if a.codeVia != "" {
			s += "\n" + a.codeVia + "\n"
		}
		s += "\n[esc] back  [ctrl+r] send a new code\n"
	case authModeLoading:
		if a.authErr == "" {
			s += "Signed in. Loading your chats...\n"
		} else {
			s += "Signed in, but the account could not be loaded.\n\n[enter] try again  [q] quit\n"
		}
	}
	return s + a.authErrView()
}

func (a App) authErrView() string {
	if a.authErr == "" {
		return ""
	}
	return "\n⚠️  " + a.authErr + "\n"
}

// qrView lays out the QR login screen. A clipped QR code cannot be scanned, so
// on a short terminal it drops the title and spacing, and when even the bare
// code does not fit it asks for a bigger window instead.
func (a App) qrView() string {
	if a.qrText == "" {
		return appTitle() + "\n\nRequesting QR code...\n\n[esc] back\n" + a.authErrView()
	}
	full := appTitle() + "\n\n" + a.qrText + "\n\n" +
		"On a phone signed in to this account, open\n" +
		"Settings → Devices → Link Desktop Device and scan the code.\n" +
		"The code refreshes automatically.\n\n[esc] back\n" + a.authErrView()
	compact := a.qrText + "\nScan it in Telegram: Settings → Devices · [esc] back"
	switch {
	case a.fits(full):
		return full
	case a.fits(compact):
		return compact
	default:
		return appTitle() + "\n\n" + fmt.Sprintf(
			"The terminal window is too small to show the QR code.\n"+
				"Enlarge it to at least %d columns by %d rows,\n"+
				"or press [esc] and sign in with a login code.\n",
			lipgloss.Width(compact), lipgloss.Height(compact))
	}
}

// fits reports whether s can be shown without clipping. The terminal size is
// unknown until the first WindowSizeMsg; until then everything fits.
func (a App) fits(s string) bool {
	if a.termHeight == 0 {
		return true
	}
	s = strings.TrimRight(s, "\n")
	return lipgloss.Height(s) <= a.termHeight && lipgloss.Width(s) <= a.termWidth
}

func (a App) View() tea.View {
	var v tea.View
	v.AltScreen = true

	switch a.screen {
	case screenPicker:
		s := appTitle() + "\n\n"
		if a.pickerConfirm != "" {
			s += fmt.Sprintf("  Delete account %s from this app?\n\n"+
				"  Its saved session will be removed. Telegram keeps listing this\n"+
				"  device in active sessions until you terminate it there.\n\n"+
				"  Press [y] to confirm, [esc] to cancel\n", a.pickerConfirm)
		} else {
			s += a.pickerList.View()
		}
		v.SetContent(s)
	case screenAuth:
		v.SetContent(a.authView())
	case screenPassword:
		s := appTitle() + "\n\nEnter your 2FA password:\n\n" + a.passInput.View() + "\n"
		if a.authErr != "" {
			s += "\n⚠️  " + a.authErr + "\n"
		}
		v.SetContent(s)
	case screenGroupList:
		content := appTitle() + "\n\n"
		var filterBar strings.Builder
		filterBar.WriteString("  Filter [Tab]: ")
		fc := a.filterCount()
		for i := range fc {
			name := filterNames[i]
			if i == a.filter {
				filterBar.WriteString("[" + name + "]")
			} else {
				filterBar.WriteString(" " + name + " ")
			}
			if i < len(filterNames)-1 {
				filterBar.WriteString(" ")
			}
		}
		content += filterBar.String() + "\n"
		if a.loadingMessages && a.loadTotal > 0 {
			pct := float64(a.loadedCount) / float64(a.loadTotal)
			content += fmt.Sprintf("  Counting messages... %d/%d\n", a.loadedCount, a.loadTotal)
			content += "  " + a.progress.ViewAs(pct) + "\n"
		}
		if a.loadingLeft {
			if a.leftCounted > 0 {
				content += fmt.Sprintf("  Left channels: counting messages... %d/%d\n", a.leftCounted, a.leftFound)
			} else {
				content += fmt.Sprintf("  Loading left channels... found %d\n", a.leftFound)
			}
		}
		if !a.loadingMessages && a.countErrors > 0 {
			content += fmt.Sprintf("  ⚠️  %d chats failed to count\n", a.countErrors)
		}
		if a.groupListErr != "" {
			content += "  ⚠️  " + a.groupListErr + "\n"
		}
		content += "\n" + a.list.View()
		content += "\n" + a.statusBar()
		v.SetContent(content)
	case screenActions:
		v.SetContent(a.actionList.View())
	case screenMessages:
		content := a.msgList.View()
		if a.msgErr != "" {
			content += "\n  ⚠️  " + a.msgErr + "\n"
		}
		v.SetContent(content)
	case screenConfirm:
		v.SetContent("\n  " + a.confirmText + "\n")
	case screenDeleting:
		content := "\n  " + a.deletingStatus + "\n"
		if a.deletingPct > 0 && !a.deletingDone {
			content += "\n  " + a.progress.ViewAs(a.deletingPct) + "\n"
		}
		if a.deletingErr != "" {
			content += "\n  Error: " + a.deletingErr + "\n"
		}
		if a.deletingDone {
			content += "\n  Press [Enter] or [Esc] to go back.\n"
		}
		v.SetContent(content)
	}
	return v
}

func (g Group) Title() string {
	if g.isLeft {
		return tagLeft + " " + g.title
	}
	switch g.chatType {
	case tg.ChatTypeGroup:
		return tagGroup + " " + g.title
	case tg.ChatTypeSupergroup:
		return tagSupergroup + " " + g.title
	case tg.ChatTypeChannel:
		return tagChannel + " " + g.title
	default:
		return g.title
	}
}
func (g Group) Description() string { return g.desc }
func (g Group) FilterValue() string { return g.title }
