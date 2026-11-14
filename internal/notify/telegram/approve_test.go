package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

// An approval is signed by a paired device. Telegram can deny, answer a
// question and ask for changes to a plan; it never allows a tool or
// approves a plan, whatever button data it is sent.
func TestTelegramNeverApproves(t *testing.T) {
	f := newFixture(t)
	r := f.open(permissionReq("r_sign000001", "git push origin main"))
	s := f.notifyRequest(r)
	link := "https://studio.tail1.ts.net/r/r_sign000001"
	var allowLink bool
	for _, b := range s.buttons() {
		if b.CallbackData != "" && !strings.Contains(b.Text, "Deny") {
			t.Errorf("button %q answers from Telegram", b.Text)
		}
		if strings.Contains(b.Text, "Allow") && b.URL == link {
			allowLink = true
		}
	}
	if !allowLink {
		t.Errorf("no Allow link to the app: %+v", s.buttons())
	}
	// Buttons sent by an older version carried these actions.
	for _, action := range []string{"allow", "session", "approve"} {
		tok, err := f.bot.db.mint(bg, chatID, r.ID, action, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.bot.db.bind(bg, []string{tok}, chatID, s.ID); err != nil {
			t.Fatal(err)
		}
		f.bot.Handle(bg, press(chatID, chatID, s.ID, tok))
		if d := f.decision(r.ID); d != nil {
			t.Fatalf("%s button decided from Telegram: %+v", action, d)
		}
		if a := f.mock.lastAnswer(t); !strings.Contains(a.Text, "app") {
			t.Errorf("%s: answer = %+v", action, a)
		}
	}
	// Deny still takes one tap.
	f.bot.Handle(bg, press(chatID, chatID, s.ID, button(t, parseButtons(f.mock.lastEdit(t).Markup), "Deny")))
	if d := f.decision(r.ID); d == nil || d.Behavior != api.BehaviorDeny {
		t.Fatalf("deny = %+v", d)
	}

	// A plan is approved in the app; changes can be asked for here.
	p := f.open(api.Request{ID: "r_sign000002", Checkpoint: api.CheckpointPlan, Tool: "ExitPlanMode", Plan: &api.Plan{Markdown: "Plan"}})
	ps := f.notifyRequest(p)
	for _, b := range ps.buttons() {
		if b.CallbackData != "" && !strings.Contains(b.Text, "Request changes") {
			t.Errorf("plan button %q answers from Telegram", b.Text)
		}
	}
	tok, _ := f.bot.db.mint(bg, chatID, p.ID, "approve", time.Now().Add(time.Minute))
	_ = f.bot.db.bind(bg, []string{tok}, chatID, ps.ID)
	f.bot.Handle(bg, press(chatID, chatID, ps.ID, tok))
	if d := f.decision(p.ID); d != nil {
		t.Fatalf("plan approved from Telegram: %+v", d)
	}
}

// Without a link to the app the message says where an approval happens.
func TestTelegramSaysWhereToAllow(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.PublicURL = func() string { return "" } })
	r := f.open(permissionReq("r_sign000003", "make deploy"))
	s := f.notifyRequest(r)
	if bs := s.buttons(); len(bs) != 1 || !strings.Contains(bs[0].Text, "Deny") {
		t.Fatalf("buttons = %+v", bs)
	}
	if !strings.Contains(s.Text, "To allow it") {
		t.Fatalf("no hint:\n%s", s.Text)
	}
}
