package whatsapp

import (
	"context"
	"sort"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
	waerrors "github.com/codebyoketch/wa-cli/internal/errors"
)

// memberRef is one group participant as cached; names are resolved when
// the list is asked for, since they change.
type memberRef struct {
	JID   string
	Admin bool
}

// groupsMaxAge is how long the joined-groups list is trusted before a
// reconnect fetches it again. Changes in between arrive as events.
const groupsMaxAge = 6 * time.Hour

// loadGroups fetches every group you're in (one request) and records
// their subjects, timers and members. Unless force is set it does
// nothing when the list was loaded recently.
func (c *Client) loadGroups(ctx context.Context, force bool) {
	if c.WA == nil {
		return
	}
	c.groupMu.Lock()
	if c.groupsBusy || (!force && !c.groupsLoaded.IsZero() && time.Since(c.groupsLoaded) < groupsMaxAge) {
		c.groupMu.Unlock()
		return
	}
	c.groupsBusy = true
	c.groupMu.Unlock()

	groups, err := c.WA.GetJoinedGroups(ctx)

	c.groupMu.Lock()
	c.groupsBusy = false
	if err == nil {
		c.groupsLoaded = time.Now()
	}
	c.groupMu.Unlock()
	if err != nil {
		c.log.Warnf("loading groups failed: %v", err)
		return
	}

	changed := false
	for _, g := range groups {
		if c.rememberGroup(g) {
			changed = true
		}
	}
	// The request also taught whatsmeow many members' names and numbers.
	c.invalidateNames()
	if changed {
		c.emit(ChatEvent{})
	}
}

// rememberGroup records a group's authoritative subject and timer in the
// chat store (creating the chat if it isn't there yet) and caches its
// members. Reports whether the chat store changed.
func (c *Client) rememberGroup(info *types.GroupInfo) (changed bool) {
	if info == nil || info.JID.IsEmpty() {
		return false
	}
	jid := info.JID.String()

	members := make([]memberRef, 0, len(info.Participants))
	for _, p := range info.Participants {
		if c.isMe(p.JID) || c.isMe(p.PhoneNumber) || c.isMe(p.LID) {
			continue
		}
		members = append(members, memberRef{JID: p.JID.ToNonAD().String(), Admin: p.IsAdmin || p.IsSuperAdmin})
	}
	c.groupMu.Lock()
	if c.groupMembers == nil {
		c.groupMembers = map[string][]memberRef{}
	}
	c.groupMembers[jid] = members
	c.groupMu.Unlock()

	if c.chats == nil {
		return false
	}
	var timer int64
	if info.IsEphemeral {
		timer = int64(info.DisappearingTimer)
	}
	chat, ok, err := c.chats.Get(jid)
	if err != nil {
		return false
	}
	if !ok {
		if err := c.chats.Upsert(chatstore.Chat{JID: jid, Name: info.Name, IsGroup: true}); err != nil {
			c.log.Warnf("storing group %s failed: %v", jid, err)
			return false
		}
		changed = true
	} else if info.Name != "" && chat.Name != info.Name {
		if err := c.chats.SetName(jid, info.Name); err != nil {
			c.log.Warnf("renaming group %s failed: %v", jid, err)
		} else {
			changed = true
		}
	}
	if chat.Ephemeral != timer {
		if err := c.chats.SetEphemeral(jid, timer); err == nil {
			changed = true
		}
	}
	return changed
}

// cachedMembers returns a group's cached member list, and whether it is
// loaded.
func (c *Client) cachedMembers(groupJID string) ([]memberRef, bool) {
	c.groupMu.Lock()
	defer c.groupMu.Unlock()
	members, ok := c.groupMembers[groupJID]
	return members, ok
}

// fetchMembers loads a group's member list from WhatsApp into the cache.
func (c *Client) fetchMembers(ctx context.Context, groupJID string) error {
	jid, err := types.ParseJID(groupJID)
	if err != nil || jid.Server != types.GroupServer {
		return waerrors.New("not a group: " + groupJID)
	}
	fetch := c.groupInfoOverride
	if fetch == nil {
		if c.WA == nil {
			return waerrors.New("not connected")
		}
		fetch = c.WA.GetGroupInfo
	}
	info, err := fetch(ctx, jid)
	if err != nil {
		return waerrors.Wrap(err, "loading group members")
	}
	if c.rememberGroup(info) {
		c.emit(ChatEvent{JID: groupJID})
	}
	return nil
}

// forgetMembers drops a group's cached member list so the next Members
// call (or a deletion that has to be checked) fetches it again.
func (c *Client) forgetMembers(groupJID string) {
	c.groupMu.Lock()
	delete(c.groupMembers, groupJID)
	c.groupMu.Unlock()
}

// Members returns a group's participants (not you), sorted by name. The
// list comes from the cache filled when connecting; a group that isn't
// cached is fetched from WhatsApp.
func (c *Client) Members(ctx context.Context, groupJID string) ([]Member, error) {
	refs, ok := c.cachedMembers(groupJID)
	if !ok {
		if err := c.fetchMembers(ctx, groupJID); err != nil {
			return nil, err
		}
		refs, _ = c.cachedMembers(groupJID)
	}

	out := make([]Member, 0, len(refs))
	for _, ref := range refs {
		out = append(out, Member{JID: ref.JID, Name: c.ResolveName(ctx, ref.JID), IsAdmin: ref.Admin})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// handleGroupInfo mirrors a change to a group's subject, timer or
// membership.
func (c *Client) handleGroupInfo(evt *events.GroupInfo) {
	jid := evt.JID.String()
	changed := false
	if evt.Name != nil && evt.Name.Name != "" && c.chats != nil {
		if chat, ok, err := c.chats.Get(jid); err == nil && ok && chat.Name != evt.Name.Name {
			if err := c.chats.SetName(jid, evt.Name.Name); err != nil {
				c.log.Warnf("renaming group %s failed: %v", jid, err)
			} else {
				changed = true
			}
		}
	}
	if evt.Ephemeral != nil {
		var timer int64
		if evt.Ephemeral.IsEphemeral {
			timer = int64(evt.Ephemeral.DisappearingTimer)
		}
		if c.setEphemeral(jid, timer) {
			changed = true
		}
	}
	if len(evt.Join)+len(evt.Leave)+len(evt.Promote)+len(evt.Demote) > 0 {
		c.forgetMembers(jid)
	}
	if changed {
		c.emit(ChatEvent{JID: jid})
	}
}
