// Package whatsapp wraps go.mau.fi/whatsmeow with a smaller,
// wa-cli-shaped API rather than leaking whatsmeow's types everywhere.
// This is the one package that talks to WhatsApp's servers; everything
// under cmd/ and the TUI go through it rather than importing whatsmeow
// directly.
//
// Client keeps the local chat and message stores (internal/chatstore,
// internal/msgstore) current from WhatsApp's events and turns them into
// the views the TUI draws; api.go is its contract with the TUI. The
// files are split by concern:
//
//	client.go   construction, lifecycle, the CLI's group/contact calls
//	events.go   whatsmeow events: receipts, presence, chat settings, ...
//	ingest.go   storing live messages and history syncs, notifications
//	fold.go     reactions/edits/deletes/votes/HD copies -> their message
//	content.go  what each WhatsApp message type is and how it reads
//	view.go     ChatView / MessageView for the TUI
//	send.go     sending, reacting, editing, deleting, forwarding
//	sendfile.go choosing how to send a file and preparing images
//	media.go    thumbnails and saving attachments
//	chatstate.go read state, pin/archive/mute, presence, older history
//	jid.go      one chat per person: phone-number vs @lid addresses
//	names.go    display names
package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	waerrors "github.com/codebyoketch/wa-cli/internal/errors"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/notify"
	"github.com/codebyoketch/wa-cli/internal/qr"
	"github.com/codebyoketch/wa-cli/internal/safety"
)

// Client is wa-cli's WhatsApp client: a higher-level wrapper around a
// *whatsmeow.Client (exported as WA) plus the local chatstore/msgstore
// state and notification settings needed to serve wa-cli's commands and
// TUI. Construct one with New. All methods are safe for concurrent use.
type Client struct {
	WA    *whatsmeow.Client
	log   waLog.Logger
	chats *chatstore.Store
	msgs  *msgstore.Store

	// Desktop notifications are opt-in (default off) — only wa watch
	// and the TUI call SetNotifications to enable them. Without this,
	// any brief background sync that also passes chats/msgs to New()
	// (e.g. `wa chat list`, `wa chat send`) would start popping
	// notifications too, which isn't what those commands are for.
	notifyEnabled     bool
	notifyGroups      bool
	notifyShowPreview bool

	// evMu guards the event handlers and behavior.
	evMu       sync.RWMutex
	onEvent    func(Event)
	onIncoming func(msgstore.Message)
	behavior   Behavior
	wantOnline bool

	// foldMu serializes the read-then-write steps of ingesting a message.
	foldMu sync.Mutex

	nameMu    sync.Mutex
	nameCache map[string]nameEntry
	nameGen   uint64 // bumped on every invalidation

	namesMu      sync.Mutex
	namesLast    time.Time
	namesPending bool

	mergeMu    sync.Mutex
	mergedLIDs map[string]struct{}

	// groupMu guards the group member cache and the deletions waiting
	// for it.
	groupMu        sync.Mutex
	groupMembers   map[string][]memberRef
	groupsLoaded   time.Time
	groupsBusy     bool
	pendingRevokes map[string][]pendingRevoke // group -> deletions to check once its members are known

	// background counts goroutines started while ingesting, so tests can
	// wait for them.
	background sync.WaitGroup

	// sendMu guards the bookkeeping of outgoing messages.
	sendMu       sync.Mutex
	pendingFiles map[string]Outgoing // message ID -> what to upload again on Retry
	lastRead     map[string]string   // chat -> newest message a read receipt went out for

	// Test seams: stand-ins for the parts of whatsmeow the local logic
	// reads. Nil means "use WA".
	lidOverride       lidSource
	contactOverride   contactSource
	decryptOverride   secretDecrypter
	groupInfoOverride func(context.Context, types.JID) (*types.GroupInfo, error)
	selfJID           types.JID
	selfLID           types.JID
	notifyFn          func(title, body string) error
}

// New builds a Client using the first (or a fresh, unpaired) device from
// container. chats/msgs may be nil for commands that don't need chat or
// message history (e.g. login/logout/status).
//
// For the TUI: call OnEvent, SetBehavior and SetNotifications, then
// Start; Stop when done.
func New(ctx context.Context, container *sqlstore.Container, log waLog.Logger, chats *chatstore.Store, msgs *msgstore.Store) (*Client, error) {
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, waerrors.Wrap(err, "loading device store")
	}
	clientLog := newQuietLogger(waLog.Stdout("Client", "WARN", true))
	waClient := whatsmeow.NewClient(device, clientLog)
	c := &Client{WA: waClient, log: log, chats: chats, msgs: msgs, wantOnline: true}

	if chats != nil || msgs != nil {
		waClient.AddEventHandler(c.handleEvent)
	}

	return c, nil
}

// OnEvent registers the single handler told about everything the TUI
// should react to (see Event). It is called from background goroutines,
// possibly several at once: the handler must be quick and safe for
// concurrent use. A second call replaces the first.
func (c *Client) OnEvent(fn func(Event)) {
	c.evMu.Lock()
	c.onEvent = fn
	c.evMu.Unlock()
}

// emit delivers e to the registered handler, if any.
func (c *Client) emit(e Event) {
	c.evMu.RLock()
	fn := c.onEvent
	c.evMu.RUnlock()
	if fn != nil {
		fn(e)
	}
}

// SetBehavior sets the user's privacy-affecting choices. The zero value
// (what a new Client has) sends neither read receipts nor presence.
func (c *Client) SetBehavior(b Behavior) {
	c.evMu.Lock()
	old := c.behavior
	c.behavior = b
	online := c.wantOnline
	c.evMu.Unlock()

	if old.SendPresence == b.SendPresence || !c.connected() {
		return
	}
	// Presence was switched while connected: show or hide you right away.
	state := types.PresenceUnavailable
	if b.SendPresence && online {
		state = types.PresenceAvailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.WA.SendPresence(ctx, state); err != nil && !errors.Is(err, whatsmeow.ErrNoPushName) {
		c.log.Warnf("updating presence failed: %v", err)
	}
}

func (c *Client) getBehavior() Behavior {
	c.evMu.RLock()
	defer c.evMu.RUnlock()
	return c.behavior
}

// OnIncomingMessage registers fn to be called for every new message
// stored from a live event. Superseded by OnEvent; kept for callers that
// only want new messages. Only one callback is supported; a second call
// replaces the first.
func (c *Client) OnIncomingMessage(fn func(msgstore.Message)) {
	c.evMu.Lock()
	c.onIncoming = fn
	c.evMu.Unlock()
}

// SetNotifications turns on desktop notifications for incoming messages.
// Off by default — see the Client struct's notify* field comments for
// why this needs to be explicit rather than inferred from chats/msgs
// being present.
func (c *Client) SetNotifications(enabled, groups, showPreview bool) {
	c.evMu.Lock()
	c.notifyEnabled = enabled
	c.notifyGroups = groups
	c.notifyShowPreview = showPreview
	c.evMu.Unlock()
}

// sendNotification shows a desktop notification in the background: the
// OS toast call can be slow, and callers run on whatsmeow's goroutines.
func (c *Client) sendNotification(title, body string) {
	fn := c.notifyFn
	if fn == nil {
		fn = notify.Send
	}
	go func() {
		if err := fn(title, body); err != nil {
			c.log.Warnf("desktop notification failed: %v", err)
		}
	}()
}

// connected reports whether there is a live, authenticated connection.
func (c *Client) connected() bool {
	return c.WA != nil && c.WA.IsConnected() && c.WA.IsLoggedIn()
}

// Start prepares the local stores and connects, for a long-running
// session such as the TUI. It first runs the local maintenance (folding
// rows imported from the old store, merging chats stored under a
// person's two addresses, mirroring the phone's pin/archive/mute state),
// then connects; whatsmeow reconnects by itself from then on, including
// when the network is down at startup. Progress arrives as ConnEvents.
// The connection is closed when ctx is cancelled or Stop is called.
func (c *Client) Start(ctx context.Context) error {
	if !c.IsLoggedIn() {
		return waerrors.ErrNotLoggedIn
	}
	c.Maintain(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}

	c.emit(ConnEvent{State: ConnConnecting})
	c.WA.InitialAutoReconnect = true
	if err := c.WA.Connect(); err != nil && !errors.Is(err, whatsmeow.ErrAlreadyConnected) {
		return waerrors.Wrap(err, "connecting to WhatsApp")
	}
	go func() {
		<-ctx.Done()
		c.Stop()
	}()
	return nil
}

// Maintain runs the local, offline upkeep of the stores. Start calls it;
// it is cheap to call again.
func (c *Client) Maintain(ctx context.Context) {
	changed := c.mergeLIDChats() > 0
	if err := c.foldLegacy(ctx); err != nil {
		c.log.Warnf("folding legacy messages failed (will retry on next start): %v", err)
	}
	if c.seedChatSettings(ctx) {
		changed = true
	}
	if changed {
		c.emit(ChatEvent{})
	}
}

// Stop closes the connection opened by Start. The session stays valid.
func (c *Client) Stop() {
	if c.WA == nil {
		return
	}
	if c.connected() && c.getBehavior().SendPresence {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.WA.SendPresence(ctx, types.PresenceUnavailable)
		cancel()
	}
	c.WA.Disconnect()
}

// statusBroadcastJID is WhatsApp's pseudo-chat for Status/story updates.
const statusBroadcastJID = "status@broadcast"

// newsletterSuffix marks WhatsApp Channel subscriptions (e.g.
// "120363...@newsletter"), unlike status@broadcast these don't have one
// fixed JID, so they're matched by suffix instead of exact equality.
const newsletterSuffix = "@newsletter"

// isPseudoChat reports whether jid is a non-conversation pseudo-chat
// (Status broadcasts, Channel/newsletter subscriptions) that shouldn't be
// printed or tracked as a real chat.
func isPseudoChat(jid string) bool {
	return jid == statusBroadcastJID || strings.HasSuffix(jid, newsletterSuffix)
}

// SyncChats connects, waits up to timeout for chat data to arrive via
// history sync / messages, then disconnects. Intended for one-shot
// commands like `wa chat list` that need reasonably current data without
// a long-running connection (that's what `wa watch` is for).
func (c *Client) SyncChats(ctx context.Context, timeout time.Duration) error {
	if !c.IsLoggedIn() {
		return waerrors.ErrNotLoggedIn
	}
	if c.chats == nil {
		return waerrors.New("SyncChats called without a chatstore")
	}

	if err := c.WA.Connect(); err != nil {
		return waerrors.Wrap(err, "connecting to WhatsApp")
	}
	defer c.WA.Disconnect()

	select {
	case <-time.After(timeout):
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// IsLoggedIn reports whether the underlying device store already has a
// persisted session, without opening a network connection.
func (c *Client) IsLoggedIn() bool {
	return c.WA != nil && c.WA.Store != nil && c.WA.Store.ID != nil
}

// Contact is a simplified view of a WhatsApp contact.
type Contact struct {
	JID          string `json:"jid"`
	Name         string `json:"name,omitempty"` // best available: FullName > FirstName > PushName > BusinessName
	PushName     string `json:"pushName,omitempty"`
	BusinessName string `json:"businessName,omitempty"`
}

// ListContacts returns all locally known contacts, sorted by name. This
// reads directly from the local device store — no network connection
// needed, since WhatsApp syncs your contact list to the device during
// login and keeps it updated from there.
func (c *Client) ListContacts(ctx context.Context) ([]Contact, error) {
	contacts, err := c.WA.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, waerrors.Wrap(err, "loading contacts")
	}

	out := make([]Contact, 0, len(contacts))
	for jid, info := range contacts {
		out = append(out, Contact{
			JID:          jid.String(),
			Name:         bestContactName(info),
			PushName:     info.PushName,
			BusinessName: info.BusinessName,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func bestContactName(info types.ContactInfo) string {
	switch {
	case info.FullName != "":
		return info.FullName
	case info.FirstName != "":
		return info.FirstName
	case info.PushName != "":
		return info.PushName
	case info.BusinessName != "":
		return info.BusinessName
	default:
		return ""
	}
}

// Group is a simplified view of a WhatsApp group.
type Group struct {
	JID              string `json:"jid"`
	Name             string `json:"name"`
	Topic            string `json:"topic,omitempty"`
	ParticipantCount int    `json:"participantCount"`
}

// Participant is one member of a group.
type Participant struct {
	JID          string `json:"jid"`
	IsAdmin      bool   `json:"isAdmin,omitempty"`
	IsSuperAdmin bool   `json:"isSuperAdmin,omitempty"`
}

// ListGroups returns all groups the account is currently a member of.
func (c *Client) ListGroups(ctx context.Context) ([]Group, error) {
	groups, err := c.WA.GetJoinedGroups(ctx)
	if err != nil {
		return nil, waerrors.Wrap(err, "loading groups")
	}

	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, Group{
			JID:              g.JID.String(),
			Name:             g.GroupName.Name,
			Topic:            g.Topic,
			ParticipantCount: len(g.Participants),
		})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// GroupInfo returns details and the participant list for one group.
func (c *Client) GroupInfo(ctx context.Context, jid types.JID) (Group, []Participant, error) {
	info, err := c.WA.GetGroupInfo(ctx, jid)
	if err != nil {
		return Group{}, nil, waerrors.Wrap(err, "loading group info")
	}

	group := Group{
		JID:              info.JID.String(),
		Name:             info.GroupName.Name,
		Topic:            info.Topic,
		ParticipantCount: len(info.Participants),
	}

	participants := make([]Participant, 0, len(info.Participants))
	for _, p := range info.Participants {
		participants = append(participants, Participant{
			JID:          p.JID.String(),
			IsAdmin:      p.IsAdmin,
			IsSuperAdmin: p.IsSuperAdmin,
		})
	}
	return group, participants, nil
}

// CreateGroup creates a new group with the given name and participants.
func (c *Client) CreateGroup(ctx context.Context, name string, participants []types.JID) (Group, error) {
	info, err := c.WA.CreateGroup(ctx, whatsmeow.ReqCreateGroup{
		Name:         name,
		Participants: participants,
	})
	if err != nil {
		return Group{}, waerrors.Wrap(err, "creating group")
	}
	return Group{
		JID:              info.JID.String(),
		Name:             info.GroupName.Name,
		Topic:            info.Topic,
		ParticipantCount: len(info.Participants),
	}, nil
}

// AddParticipants adds participants to an existing group.
func (c *Client) AddParticipants(ctx context.Context, groupJID types.JID, participants []types.JID) error {
	_, err := c.WA.UpdateGroupParticipants(ctx, groupJID, participants, whatsmeow.ParticipantChangeAdd)
	if err != nil {
		return waerrors.Wrap(err, "adding participants")
	}
	return nil
}

// RemoveParticipants removes participants from a group.
func (c *Client) RemoveParticipants(ctx context.Context, groupJID types.JID, participants []types.JID) error {
	_, err := c.WA.UpdateGroupParticipants(ctx, groupJID, participants, whatsmeow.ParticipantChangeRemove)
	if err != nil {
		return waerrors.Wrap(err, "removing participants")
	}
	return nil
}

// Connect establishes the WhatsApp connection. whatsmeow's WA.Connect()
// only *initiates* the handshake and returns immediately — a request
// sent right after can race a socket that isn't ready yet. Requests that
// may be sent right after connecting wait for the connection themselves
// (see awaitConnection) rather than sleeping here.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.WA.Connect(); err != nil {
		return waerrors.Wrap(err, "connecting to WhatsApp")
	}
	return nil
}

// Login connects and, if unpaired, shows a QR code to scan. It stays
// connected for a grace period after pairing so WhatsApp has time to
// commit the linked device and, if a chatstore is attached, deliver the
// one-time HistorySync payload — disconnecting immediately after the QR
// "success" event is too early and the device never shows up in
// WhatsApp > Linked Devices.
func (c *Client) Login(ctx context.Context) error {
	if c.IsLoggedIn() {
		return c.Connect(ctx)
	}

	qrChan, err := c.WA.GetQRChannel(ctx)
	if err != nil {
		return waerrors.Wrap(err, "getting QR channel")
	}

	if err := c.WA.Connect(); err != nil {
		return waerrors.Wrap(err, "connecting to WhatsApp")
	}

	paired := false
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			fmt.Println("Scan this QR code in WhatsApp: Settings > Linked Devices > Link a Device")
			qr.Print(evt.Code)
		case "success":
			paired = true
			fmt.Println("QR scanned — finishing setup, keep this open...")
		case "timeout":
			return waerrors.New("QR code expired, run 'wa login' again")
		default:
			c.log.Infof("login event: %s", evt.Event)
		}
	}

	if !paired {
		return waerrors.New("login did not complete")
	}

	// Give WhatsApp time to finish the post-pairing handshake and, if a
	// chatstore is attached, deliver the one-time HistorySync payload.
	wait := 15 * time.Second
	if c.chats != nil {
		wait = 25 * time.Second
		fmt.Println("Syncing chat history — this can take a bit on first login...")
	} else {
		fmt.Println("Finishing setup...")
	}

	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return ctx.Err()
	}

	fmt.Println("Login successful.")
	return nil
}

// Watch keeps a long-running connection open, printing incoming messages
// and reconnecting on drops, until ctx is cancelled (e.g. Ctrl+C).
func (c *Client) Watch(ctx context.Context, guard *safety.Guard) error {
	if !c.IsLoggedIn() {
		return waerrors.ErrNotLoggedIn
	}

	c.WA.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			c.printIncoming(v, guard)
		case *events.Connected:
			fmt.Println("[connected]")
		case *events.Disconnected:
			fmt.Println("[disconnected — attempting to reconnect...]")
			go c.reconnectWithBackoff(ctx)
		case *events.LoggedOut:
			fmt.Println("[logged out remotely — run 'wa login' again]")
		}
	})

	if err := c.WA.Connect(); err != nil {
		return waerrors.Wrap(err, "connecting to WhatsApp")
	}
	defer c.WA.Disconnect()

	fmt.Println("Watching for new messages. Press Ctrl+C to stop.")

	<-ctx.Done()
	fmt.Println("\nStopping...")
	return nil
}

// reconnectWithBackoff is a fallback alongside whatsmeow's own
// reconnect handling: it retries WA.Connect() with exponential backoff
// (capped at 30s) until there is a connection again — whoever made it —
// or ctx is cancelled.
func (c *Client) reconnectWithBackoff(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if c.WA.IsConnected() {
			return // whatsmeow's own reconnect got there first
		}
		err := c.WA.Connect()
		if err == nil {
			fmt.Println("[reconnected]")
			return
		}
		if errors.Is(err, whatsmeow.ErrAlreadyConnected) {
			return
		}
		c.log.Warnf("reconnect attempt failed: %v", err)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// printIncoming prints one incoming message and marks its sender known
// (so a later `wa chat send` reply doesn't trigger the new-recipient
// confirmation prompt).
func (c *Client) printIncoming(evt *events.Message, guard *safety.Guard) {
	if evt.Info.IsFromMe || isPseudoChat(evt.Info.Chat.String()) {
		return
	}
	a := analyze(unwrapMessage(evt.Message))
	if a.Class != classRow {
		return // a reaction, edit, delete or plumbing: not a message to print
	}

	sender := evt.Info.PushName
	if sender == "" {
		sender = evt.Info.Sender.User
	}

	text := a.Text
	switch a.Kind {
	case KindText, KindSystem, KindUnsupported, KindLocation, KindContact:
	case KindPoll:
		text = withMediaTag("poll", text)
	default:
		text = withMediaTag(a.MediaType, text)
	}

	ts := evt.Info.Timestamp.Local().Format("15:04:05")
	fmt.Printf("[%s] %s (%s): %s\n", ts, sender, evt.Info.Chat.String(), text)

	if guard != nil {
		if err := guard.MarkKnown(evt.Info.Sender.String()); err != nil {
			c.log.Warnf("failed to mark sender known: %v", err)
		}
	}
}

// Logout terminates the linked-device session on WhatsApp's servers and
// clears the local session, so a subsequent command needs `wa login`
// again. Returns waerrors.ErrNotLoggedIn if no session is active.
func (c *Client) Logout(ctx context.Context) error {
	if !c.IsLoggedIn() {
		return waerrors.ErrNotLoggedIn
	}
	err := c.withRetry(ctx, func() error {
		return c.WA.Logout(ctx)
	})
	if err != nil {
		return waerrors.Wrap(err, "logging out")
	}
	return nil
}

// Disconnect closes the current WhatsApp connection without invalidating
// the underlying session, so a later command can reconnect and resume
// using the same persisted device store.
func (c *Client) Disconnect() {
	c.WA.Disconnect()
}
