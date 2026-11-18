package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/codebyoketch/wa-cli/internal/msgstore"
)

// What a hostile sender or group member must not be able to do.

func TestIngest_PlaceholderIsOnlyFilledByItsSender(t *testing.T) {
	tc := newStoreClient(t)
	info := liveEvent(groupJID, ashaPN, "VO1", nil, false).Info
	tc.ingestUnavailable(&events.UndecryptableMessage{Info: info, IsUnavailable: true, UnavailableType: events.UnavailableTypeViewOnce})
	isPlaceholder := func() bool {
		return contentOf(tc.mustGet(t, groupJID, "VO1")).GetPlaceholderMessage() != nil
	}
	if !isPlaceholder() {
		t.Fatal("no placeholder stored")
	}

	// Another member reuses the ID: his words must not appear under her name.
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "VO1", text("send the money to me"), false))
	if !isPlaceholder() {
		t.Fatalf("another member's message took her placeholder's place: %+v", tc.mustGet(t, groupJID, "VO1"))
	}
	// Nor does a message of yours.
	tc.ingestMessage(liveEvent(groupJID, meJID, "VO1", text("mine"), true))
	if !isPlaceholder() {
		t.Fatalf("your message took her placeholder's place: %+v", tc.mustGet(t, groupJID, "VO1"))
	}

	// Her own does - also when it arrives under her other address.
	tc.ingestMessage(liveEvent(groupJID, ashaLID, "VO1", imageMsg("/v/once", ""), false))
	m := tc.mustGet(t, groupJID, "VO1")
	if m.MediaType != "image" || contentOf(m).GetImageMessage() == nil {
		t.Errorf("her own message did not replace the placeholder: %+v", m)
	}
}

func TestFold_GroupDeleteBySomeoneElseIsCheckedFirst(t *testing.T) {
	tc := newStoreClient(t)
	group := groupJID.String()
	tc.ingestMessage(liveEvent(groupJID, ashaPN, "M1", text("one"), false))
	tc.ingestMessage(liveEvent(groupJID, ashaPN, "M2", text("two"), false))
	deleted := func(id string) bool { return tc.mustGet(t, groupJID, id).Deleted }

	// The member list isn't loaded and can't be fetched (no connection):
	// a claim that can't be checked deletes nothing.
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "D1", revoke("M1"), false))
	tc.background.Wait()
	if deleted("M1") {
		t.Fatal("a delete from someone whose rights couldn't be checked was applied")
	}

	// It can be fetched now, and says he isn't an admin: still nothing.
	admin, fetches := false, 0
	release := make(chan struct{})
	close(release)
	tc.groupInfoOverride = func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
		<-release
		fetches++
		return &types.GroupInfo{JID: jid, Participants: []types.GroupParticipant{
			{JID: ashaPN}, {JID: bilalPN, IsAdmin: admin},
		}}, nil
	}
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "D2", revoke("M1"), false))
	tc.background.Wait()
	if deleted("M1") || fetches != 1 {
		t.Fatalf("a non-admin's delete: deleted=%v after %d fetch(es)", deleted("M1"), fetches)
	}

	// A flood of them while the list is being fetched costs one fetch.
	tc.forgetMembers(group)
	release = make(chan struct{})
	for i := 0; i < 3*maxPendingRevokes; i++ {
		tc.ingestMessage(liveEvent(groupJID, bilalPN, "F"+string(rune('a'+i%26)), revoke("M1"), false))
	}
	close(release)
	tc.background.Wait()
	if deleted("M1") || fetches != 2 {
		t.Fatalf("a flood of deletes: deleted=%v after %d fetch(es), want 2", deleted("M1"), fetches)
	}

	// One read from history has nothing current to be checked against.
	tc.forgetMembers(group)
	old := tc.incomingFromEvent(liveEvent(groupJID, bilalPN, "D3", revoke("M1"), false), groupJID)
	old.History = true
	tc.ingest(context.Background(), old, "")
	tc.background.Wait()
	if deleted("M1") || fetches != 2 {
		t.Fatalf("a delete from history: deleted=%v after %d fetch(es), want 2", deleted("M1"), fetches)
	}

	// Once he is an admin, his delete is applied - after the check.
	admin = true
	tc.log.reset()
	tc.ingestMessage(liveEvent(groupJID, bilalPN, "D4", revoke("M2"), false))
	tc.background.Wait()
	if !deleted("M2") || deleted("M1") {
		t.Fatalf("an admin's delete: M2 deleted=%v, M1 deleted=%v", deleted("M2"), deleted("M1"))
	}
	if !tc.log.has(MessageEvent{ChatJID: group, ID: "M2"}) {
		t.Errorf("expected a change event for M2, got %+v", tc.log.all())
	}
}

func TestMediaFileNames_UnknownTypeGetsNoRunnableExtension(t *testing.T) {
	docx := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	cases := []struct {
		name string
		msg  *waProto.Message
		want string // the preferred name, or its ending when it starts with "*"
	}{
		{"photo", imageMsg("/v/p", ""), "*.jpg"},
		// The type a sender claims is not a reason to give a file an
		// extension that Windows runs.
		{"video claiming to be an HTML application", &waProto.Message{VideoMessage: &waProto.VideoMessage{
			Mimetype: proto.String("application/hta"), DirectPath: proto.String("/v/v"),
		}}, "*.bin"},
		{"audio claiming to be a program", &waProto.Message{AudioMessage: &waProto.AudioMessage{
			Mimetype: proto.String("application/x-msdownload"), DirectPath: proto.String("/v/a"),
		}}, "*.bin"},
		{"unnamed-type document claiming to be a program", &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
			FileName: proto.String("invoice"), Mimetype: proto.String("application/x-msdownload"), DirectPath: proto.String("/v/d1"),
		}}, "invoice.bin"},
		// A document keeps the name it was sent under.
		{"pdf", &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
			FileName: proto.String("report.pdf"), Mimetype: proto.String("application/x-msdownload"), DirectPath: proto.String("/v/d2"),
		}}, "report.pdf"},
		{"word document without an extension", &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
			FileName: proto.String("Q3 plan"), Mimetype: proto.String(docx), DirectPath: proto.String("/v/d3"),
		}}, "Q3 plan.docx"},
	}
	for _, tc := range cases {
		m := msgstore.Message{ID: "3EB0ABCDEF123456", ChatJID: ashaPN.String(), Timestamp: 1_700_000_000_000, RawProto: rawOf(tc.msg)}
		a := analyze(contentOf(m))
		if a.Media == nil {
			t.Fatalf("%s: not recognised as media", tc.name)
		}
		names := mediaFileNames(m, a)
		got := names[0]
		if ending, ok := strings.CutPrefix(tc.want, "*"); ok {
			if !strings.HasSuffix(got, ending) {
				t.Errorf("%s: saved as %q, want a name ending in %q", tc.name, got, ending)
			}
		} else if got != tc.want {
			t.Errorf("%s: saved as %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMarkFromInternet(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the mark of the web is a Windows feature")
	}
	path := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(path, []byte("%PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+":probe", nil, 0o600); err != nil {
		t.Skipf("this file system has no named streams: %v", err)
	}

	markFromInternet(path)

	mark, err := os.ReadFile(path + ":Zone.Identifier")
	if err != nil || !strings.Contains(string(mark), "ZoneId=3") {
		t.Fatalf("mark = %q, %v; want the internet zone", mark, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "%PDF" {
		t.Errorf("the file itself changed: %q", data)
	}
}
