package whatsapp

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waCommon"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/localdb"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/store"
)

// The people in these tests.
var (
	meJID  = types.NewJID("911000000001", types.DefaultUserServer)
	meLID  = types.NewJID("100000000000001", types.HiddenUserServer)
	ashaPN = types.NewJID("919541988522", types.DefaultUserServer)
	// Asha's privacy ID: the same person as ashaPN.
	ashaLID = types.NewJID("177403959709918", types.HiddenUserServer)
	bilalPN = types.NewJID("919000000002", types.DefaultUserServer)
	// A LID whose phone number isn't known.
	strangerLID = types.NewJID("200000000000009", types.HiddenUserServer)
	groupJID    = types.NewJID("120363000000000001", types.GroupServer)
)

// fakeLIDs is a LID <-> phone-number mapping for tests.
type fakeLIDs struct {
	mu      sync.Mutex
	lidToPN map[string]string // user parts
}

func (f *fakeLIDs) add(lid, pn types.JID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lidToPN == nil {
		f.lidToPN = map[string]string{}
	}
	f.lidToPN[lid.User] = pn.User
}

func (f *fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pn, ok := f.lidToPN[lid.User]; ok {
		return types.NewJID(pn, types.DefaultUserServer), nil
	}
	return types.EmptyJID, nil
}

func (f *fakeLIDs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for lid, mapped := range f.lidToPN {
		if mapped == pn.User {
			return types.NewJID(lid, types.HiddenUserServer), nil
		}
	}
	return types.EmptyJID, nil
}

// fakeContacts is an address book for tests that counts its lookups.
type fakeContacts struct {
	mu      sync.Mutex
	names   map[types.JID]types.ContactInfo
	lookups int
}

func (f *fakeContacts) set(jid types.JID, info types.ContactInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names == nil {
		f.names = map[types.JID]types.ContactInfo{}
	}
	f.names[jid] = info
}

func (f *fakeContacts) GetContact(_ context.Context, user types.JID) (types.ContactInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	return f.names[user], nil
}

func (f *fakeContacts) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

// eventLog records the events a Client emits.
type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) add(e Event) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *eventLog) all() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.events...)
}

func (l *eventLog) reset() {
	l.mu.Lock()
	l.events = nil
	l.mu.Unlock()
}

// messageEvents returns the MessageEvents recorded so far.
func (l *eventLog) messageEvents() []MessageEvent {
	var out []MessageEvent
	for _, e := range l.all() {
		if me, ok := e.(MessageEvent); ok {
			out = append(out, me)
		}
	}
	return out
}

// has reports whether an event equal to want was recorded.
func (l *eventLog) has(want Event) bool {
	for _, e := range l.all() {
		if e == want {
			return true
		}
	}
	return false
}

// testClient is a Client on real stores in a temp dir, with stubbed
// address book and LID mapping and no connection.
type testClient struct {
	*Client
	lids     *fakeLIDs
	contacts *fakeContacts
	log      *eventLog
	dir      string
}

func newStoreClient(t *testing.T) *testClient {
	t.Helper()
	dir := t.TempDir()
	tc := &testClient{
		lids:     &fakeLIDs{},
		contacts: &fakeContacts{},
		log:      &eventLog{},
		dir:      dir,
	}
	tc.lids.add(ashaLID, ashaPN)
	tc.lids.add(meLID, meJID)
	tc.Client = &Client{
		log:             testLogger(),
		chats:           chatstore.New(dir),
		msgs:            msgstore.New(dir),
		lidOverride:     tc.lids,
		contactOverride: tc.contacts,
		selfJID:         meJID,
		selfLID:         meLID,
	}
	tc.OnEvent(tc.log.add)
	// Windows can't remove the temp dir while the database is open.
	t.Cleanup(func() { localdb.Close(dir) })
	return tc
}

// newRealClient is a Client built by New on a real (offline, never
// connected) whatsmeow device store in a temp dir, for the paths that go
// through whatsmeow itself: parsing history messages and decrypting poll
// votes.
func newRealClient(t *testing.T) *testClient {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	container, err := store.Open(ctx, dir, waLog.Noop)
	if err != nil {
		t.Fatalf("opening device store: %v", err)
	}
	t.Cleanup(func() {
		localdb.Close(dir)
		container.Close()
	})

	device := container.NewDevice()
	own := meJID
	own.Device = 7
	device.ID = &own
	device.LID = meLID
	device.PushName = "Me"
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{},
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err := device.Save(ctx); err != nil {
		t.Fatalf("saving test device: %v", err)
	}

	client, err := New(ctx, container, testLogger(), chatstore.New(dir), msgstore.New(dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !client.IsLoggedIn() {
		t.Fatal("test device should count as logged in")
	}
	tc := &testClient{Client: client, log: &eventLog{}, dir: dir}
	tc.OnEvent(tc.log.add)
	if err := client.WA.Store.LIDs.PutLIDMapping(ctx, ashaLID, ashaPN); err != nil {
		t.Fatalf("storing LID mapping: %v", err)
	}
	return tc
}

// rawOf serializes a message the way the store keeps it.
func rawOf(msg *waProto.Message) string { return encodeRawProto(msg) }

// text builds a plain text message.
func text(s string) *waProto.Message {
	return &waProto.Message{Conversation: proto.String(s)}
}

// liveEvent builds a message event as whatsmeow would deliver it. sender
// is ignored for fromMe (it becomes your own JID).
func liveEvent(chat, sender types.JID, id string, msg *waProto.Message, fromMe bool) *events.Message {
	if fromMe {
		sender = meJID
	}
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     chat,
				Sender:   sender,
				IsFromMe: fromMe,
				IsGroup:  chat.Server == types.GroupServer,
			},
			ID:        id,
			Timestamp: time.Now(),
		},
		Message: msg,
	}
}

// mustGet returns a stored message or fails the test.
func (tc *testClient) mustGet(t *testing.T, chat types.JID, id string) msgstore.Message {
	t.Helper()
	m, ok, err := tc.msgs.Get(chat.String(), id)
	if err != nil {
		t.Fatalf("Get(%s, %s): %v", chat, id, err)
	}
	if !ok {
		t.Fatalf("message %s is not stored in %s", id, chat)
	}
	return m
}

// rowCount returns how many rows a chat has.
func (tc *testClient) rowCount(t *testing.T, chat types.JID) int {
	t.Helper()
	n, err := tc.msgs.Count(chat.String())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	return n
}

// mustChat returns a stored chat or fails the test.
func (tc *testClient) mustChat(t *testing.T, chat types.JID) chatstore.Chat {
	t.Helper()
	ch, ok, err := tc.chats.Get(chat.String())
	if err != nil {
		t.Fatalf("chats.Get(%s): %v", chat, err)
	}
	if !ok {
		t.Fatalf("chat %s is not stored", chat)
	}
	return ch
}

// key builds the key of the message an event refers to.
func key(id string) *waCommon.MessageKey {
	return &waCommon.MessageKey{ID: proto.String(id)}
}
