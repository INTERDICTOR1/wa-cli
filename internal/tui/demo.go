package tui

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// demoBackend is a pretend WhatsApp that lives entirely in memory. `wa`
// uses it when WA_DEMO=1 is set, so the interface can be tried (and
// tested end to end) without an account, a connection, or any risk of
// messaging a real person. If WA_DEMO_LOG names a file, every message
// "sent" is appended to it, one per line.
type demoBackend struct {
	mu     sync.Mutex
	chats  map[string]*whatsapp.ChatView
	msgs   map[string][]whatsapp.MessageView
	emit   func(whatsapp.Event)
	nextID int
	log    string
}

func newDemoBackend() *demoBackend {
	d := &demoBackend{
		chats: map[string]*whatsapp.ChatView{},
		msgs:  map[string][]whatsapp.MessageView{},
		emit:  func(whatsapp.Event) {},
		log:   os.Getenv("WA_DEMO_LOG"),
	}
	now := time.Now()
	add := func(jid, title string, pinned bool) {
		d.chats[jid] = &whatsapp.ChatView{
			Chat:  chatstore.Chat{JID: jid, Name: title, IsGroup: strings.HasSuffix(jid, "@g.us"), Pinned: pinned},
			Title: title,
		}
	}
	say := func(jid, sender, name, body string, ago time.Duration, mine bool) *whatsapp.MessageView {
		d.nextID++
		v := whatsapp.MessageView{
			Message: msgstore.Message{
				ID: fmt.Sprintf("demo%04d", d.nextID), ChatJID: jid, SenderJID: sender, Text: body,
				FromMe: mine, Timestamp: now.Add(-ago).UnixMilli(), Seq: int64(d.nextID), RawProto: "ZGVtbw==",
			},
			Kind: whatsapp.KindText, Sender: name, Body: body,
		}
		if mine {
			v.SenderJID, v.Sender, v.Status = "me", "", msgstore.StatusRead
		}
		d.msgs[jid] = append(d.msgs[jid], v)
		c := d.chats[jid]
		c.LastMessageAt, c.LastMessagePreview = v.Timestamp, body
		return &d.msgs[jid][len(d.msgs[jid])-1]
	}

	add("111@s.whatsapp.net", "Aisha Khan", true)
	add("222@g.us", "Weekend Trip 🏕", false)
	add("333@s.whatsapp.net", "Bilal", false)
	add("444@s.whatsapp.net", "Mum", false)

	say("111@s.whatsapp.net", "111@s.whatsapp.net", "Aisha Khan", "Did you see the photos from Saturday?", 26*time.Hour, false)
	say("111@s.whatsapp.net", "me", "", "Not yet, send them over", 25*time.Hour, true)
	photo := say("111@s.whatsapp.net", "111@s.whatsapp.net", "Aisha Khan", "The view from the top", 50*time.Minute, false)
	photo.Kind, photo.HD = whatsapp.KindImage, true
	photo.Media = &whatsapp.MediaInfo{Mime: "image/jpeg", Width: 1600, Height: 1000, Size: 420 << 10, HasThumb: true}
	reply := say("111@s.whatsapp.net", "me", "", "Wow. That's *stunning*", 45*time.Minute, true)
	reply.Quote = &whatsapp.Quote{ID: photo.ID, Sender: "Aisha Khan", Text: "[image] The view from the top"}
	reply.Reactions = []whatsapp.ReactionView{{Emoji: "❤️", Count: 1, Names: []string{"Aisha Khan"}}}

	say("222@g.us", "111@s.whatsapp.net", "Aisha Khan", "Who's driving on Friday?", 3*time.Hour, false)
	say("222@g.us", "333@s.whatsapp.net", "Bilal", "I can take four people", 170*time.Minute, false)
	poll := say("222@g.us", "333@s.whatsapp.net", "Bilal", "", 160*time.Minute, false)
	poll.Kind = whatsapp.KindPoll
	poll.Poll = &whatsapp.PollView{Question: "Leave at?", Options: []whatsapp.PollOption{{Name: "6 am", Votes: 1}, {Name: "8 am", Votes: 3, Mine: true}}}
	say("222@g.us", "me", "", "8 works for me", 150*time.Minute, true)
	d.chats["222@g.us"].UnreadCount = 2
	say("222@g.us", "111@s.whatsapp.net", "Aisha Khan", "8 it is then", 20*time.Minute, false)
	say("222@g.us", "111@s.whatsapp.net", "Aisha Khan", "Bring a jacket, it gets cold\nand a torch", 19*time.Minute, false)

	say("333@s.whatsapp.net", "333@s.whatsapp.net", "Bilal", "Call me when you're free", 2*24*time.Hour, false)
	say("444@s.whatsapp.net", "444@s.whatsapp.net", "Mum", "Dinner on Sunday?", 5*24*time.Hour, false)
	return d
}

func (d *demoBackend) onEvent(fn func(whatsapp.Event)) { d.emit = fn }

func (d *demoBackend) find(chatJID, id string) *whatsapp.MessageView {
	for i := range d.msgs[chatJID] {
		if d.msgs[chatJID][i].ID == id {
			return &d.msgs[chatJID][i]
		}
	}
	return nil
}

func (d *demoBackend) Chats(context.Context) ([]whatsapp.ChatView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]whatsapp.ChatView, 0, len(d.chats))
	for _, c := range d.chats {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastMessageAt > out[j].LastMessageAt })
	return out, nil
}

func (d *demoBackend) Chat(_ context.Context, jid string) (whatsapp.ChatView, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.chats[jid]; ok {
		return *c, true, nil
	}
	return whatsapp.ChatView{}, false, nil
}

func (d *demoBackend) Messages(_ context.Context, chatJID string, before *msgstore.Message, limit int) ([]whatsapp.MessageView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	all := d.msgs[chatJID]
	end := len(all)
	if before != nil {
		for end > 0 && !isOlder(&all[end-1].Message, before) {
			end--
		}
	}
	return append([]whatsapp.MessageView(nil), all[max(0, end-limit):end]...), nil
}

func (d *demoBackend) Message(_ context.Context, chatJID, id string) (whatsapp.MessageView, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if v := d.find(chatJID, id); v != nil {
		return *v, true, nil
	}
	return whatsapp.MessageView{}, false, nil
}

func (d *demoBackend) Search(_ context.Context, chatJID, query string, limit int) ([]whatsapp.MessageView, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []whatsapp.MessageView
	q := strings.ToLower(query)
	for jid, msgs := range d.msgs {
		if chatJID != "" && jid != chatJID {
			continue
		}
		for _, v := range msgs {
			if strings.Contains(strings.ToLower(v.Body), q) && len(out) < limit {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

func (d *demoBackend) Contacts(context.Context) ([]whatsapp.Contact, error) {
	return []whatsapp.Contact{{JID: "555@s.whatsapp.net", Name: "Zara (no chat yet)"}}, nil
}

func (d *demoBackend) Members(context.Context, string) ([]whatsapp.Member, error) {
	return []whatsapp.Member{
		{JID: "111@s.whatsapp.net", Name: "Aisha Khan", IsAdmin: true},
		{JID: "333@s.whatsapp.net", Name: "Bilal"},
	}, nil
}

func (d *demoBackend) Send(_ context.Context, chatJID string, out whatsapp.Outgoing) error {
	d.mu.Lock()
	d.nextID++
	id := fmt.Sprintf("demo%04d", d.nextID)
	v := whatsapp.MessageView{
		Message: msgstore.Message{
			ID: id, ChatJID: chatJID, SenderJID: "me", Text: out.Text, FromMe: true,
			Timestamp: time.Now().UnixMilli(), Seq: int64(d.nextID), Status: msgstore.StatusPending, RawProto: "ZGVtbw==",
		},
		Kind: whatsapp.KindText, Body: out.Text,
	}
	if out.ReplyTo != nil {
		v.Quote = &whatsapp.Quote{ID: out.ReplyTo.ID, Sender: "them", Text: out.ReplyTo.Text}
	}
	if out.FilePath != "" {
		info, _ := os.Stat(out.FilePath)
		v.Kind = whatsapp.KindDocument
		v.Media = &whatsapp.MediaInfo{FileName: filepath.Base(out.FilePath)}
		if info != nil {
			v.Media.Size = uint64(info.Size())
		}
	}
	d.msgs[chatJID] = append(d.msgs[chatJID], v)
	c, ok := d.chats[chatJID]
	if !ok {
		c = &whatsapp.ChatView{Chat: chatstore.Chat{JID: chatJID}, Title: chatJID}
		d.chats[chatJID] = c
	}
	c.LastMessageAt, c.LastMessagePreview, c.UnreadCount = v.Timestamp, out.Text, 0
	if d.log != "" {
		if f, err := os.OpenFile(d.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			fmt.Fprintf(f, "SENT to=%s text=%q file=%q\n", chatJID, out.Text, out.FilePath)
			f.Close()
		}
	}
	d.mu.Unlock()

	d.emit(whatsapp.MessageEvent{ChatJID: chatJID, ID: id, New: true})
	d.emit(whatsapp.ChatEvent{JID: chatJID})
	go func() { // the "server" accepts it, then the "recipient" reads it
		for _, st := range []msgstore.Status{msgstore.StatusSent, msgstore.StatusDelivered, msgstore.StatusRead} {
			time.Sleep(600 * time.Millisecond)
			d.mu.Lock()
			if m := d.find(chatJID, id); m != nil {
				m.Status = st
			}
			d.mu.Unlock()
			d.emit(whatsapp.MessageEvent{ChatJID: chatJID, ID: id})
		}
	}()
	return nil
}

func (d *demoBackend) change(msg msgstore.Message, fn func(*whatsapp.MessageView)) error {
	d.mu.Lock()
	v := d.find(msg.ChatJID, msg.ID)
	if v != nil {
		fn(v)
	}
	d.mu.Unlock()
	if v == nil {
		return errors.New("message not found")
	}
	d.emit(whatsapp.MessageEvent{ChatJID: msg.ChatJID, ID: msg.ID})
	return nil
}

func (d *demoBackend) Retry(context.Context, msgstore.Message) error { return nil }

func (d *demoBackend) React(_ context.Context, msg msgstore.Message, emoji string) error {
	return d.change(msg, func(v *whatsapp.MessageView) {
		v.Reactions = nil
		if emoji != "" {
			v.Reactions = []whatsapp.ReactionView{{Emoji: emoji, Count: 1, Mine: true, Names: []string{"You"}}}
		}
	})
}

func (d *demoBackend) Edit(_ context.Context, msg msgstore.Message, text string) error {
	return d.change(msg, func(v *whatsapp.MessageView) { v.Text, v.Body, v.Edited = text, text, true })
}

func (d *demoBackend) Revoke(_ context.Context, msg msgstore.Message) error {
	return d.change(msg, func(v *whatsapp.MessageView) {
		v.Deleted, v.Kind, v.Body, v.Text, v.Media, v.Poll = true, whatsapp.KindDeleted, "", "", nil, nil
	})
}

func (d *demoBackend) DeleteLocal(msg msgstore.Message) error {
	d.mu.Lock()
	msgs := d.msgs[msg.ChatJID]
	for i := range msgs {
		if msgs[i].ID == msg.ID {
			d.msgs[msg.ChatJID] = append(msgs[:i:i], msgs[i+1:]...)
			break
		}
	}
	d.mu.Unlock()
	d.emit(whatsapp.MessageEvent{ChatJID: msg.ChatJID, ID: msg.ID})
	return nil
}

func (d *demoBackend) Forward(ctx context.Context, to string, msg msgstore.Message) error {
	return d.Send(ctx, to, whatsapp.Outgoing{Text: msg.Text})
}

func (d *demoBackend) setChat(jid string, fn func(*whatsapp.ChatView)) error {
	d.mu.Lock()
	if c, ok := d.chats[jid]; ok {
		fn(c)
	}
	d.mu.Unlock()
	d.emit(whatsapp.ChatEvent{JID: jid})
	return nil
}

func (d *demoBackend) MarkChatRead(_ context.Context, jid string) error {
	return d.setChat(jid, func(c *whatsapp.ChatView) { c.UnreadCount, c.MarkedUnread = 0, false })
}
func (d *demoBackend) MarkChatUnread(_ context.Context, jid string) error {
	return d.setChat(jid, func(c *whatsapp.ChatView) { c.MarkedUnread = true })
}
func (d *demoBackend) SetPinned(_ context.Context, jid string, on bool) error {
	return d.setChat(jid, func(c *whatsapp.ChatView) { c.Pinned = on })
}
func (d *demoBackend) SetArchived(_ context.Context, jid string, on bool) error {
	return d.setChat(jid, func(c *whatsapp.ChatView) { c.Archived = on })
}
func (d *demoBackend) SetMuted(_ context.Context, jid string, on bool) error {
	return d.setChat(jid, func(c *whatsapp.ChatView) { c.Muted = on })
}
func (d *demoBackend) SetDraft(jid, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.chats[jid]; ok {
		c.Draft = text
	}
	return nil
}

func (d *demoBackend) LoadOlder(context.Context, string) (bool, error) { return false, nil }
func (d *demoBackend) SetOnline(context.Context, bool) error           { return nil }
func (d *demoBackend) SetTyping(context.Context, string, bool) error   { return nil }

func (d *demoBackend) WatchPresence(_ context.Context, jid string) error {
	if strings.HasSuffix(jid, "@s.whatsapp.net") {
		d.emit(whatsapp.PresenceEvent{JID: jid, Online: jid == "111@s.whatsapp.net", LastSeen: time.Now().Add(-40 * time.Minute).UnixMilli()})
	}
	return nil
}

// demoPicture paints a simple landscape, so the photo viewer has
// something to show at any size.
func demoPicture(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sky := uint8(120 + 100*y/h)
			c := color.RGBA{sky / 2, sky * 3 / 4, 255 - sky/4, 255}
			ridge := h/2 + (x*7%(h/3+1))/3 + abs(x-w/2)*h/(2*w)
			if y > ridge {
				c = color.RGBA{uint8(40 + 60*x/w), uint8(90 + 80*(h-y)/h), 60, 255}
			}
			if dx, dy := x-w*3/4, y-h/5; dx*dx+dy*dy < (h/9)*(h/9) {
				c = color.RGBA{255, 230, 120, 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (d *demoBackend) Thumbnail(context.Context, msgstore.Message) (image.Image, error) {
	return demoPicture(96, 60), nil
}

func (d *demoBackend) SaveMedia(_ context.Context, msg msgstore.Message, dir string) (string, error) {
	path := filepath.Join(dir, "demo-"+msg.ID+".jpg")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return path, jpeg.Encode(f, demoPicture(1600, 1000), &jpeg.Options{Quality: 90})
}
