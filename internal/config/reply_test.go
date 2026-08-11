package config

import "testing"

// TestValidateReply exercises the ntfy reply subscriber's startup gate (§8.6,
// gates 1 and 2). These are security invariants: the daemon must refuse to
// start when the reply subscriber is enabled but its topic could be guessed or
// its topic reused for read access — either would hand a remote publisher PTY
// keystroke access.
func TestValidateReply(t *testing.T) {
	base := func() Notify {
		return Notify{
			Ntfy: NotifyNtfy{
				Enabled: true,
				Topic:   "notify-topic",
				Reply: NotifyNtfyReply{
					Enabled: true,
					Topic:   "reply-topic",
					Token:   "s3cret",
				},
			},
		}
	}

	cases := []struct {
		name    string
		mutate  func(*Notify)
		wantErr bool
	}{
		{"valid", func(*Notify) {}, false},
		{"disabled reply is always ok", func(n *Notify) {
			n.Ntfy.Reply = NotifyNtfyReply{Enabled: false, Topic: "notify-topic"} // even a colliding topic is fine when off
		}, false},
		{"reply needs ntfy enabled", func(n *Notify) { n.Ntfy.Enabled = false }, true},
		{"missing token rejected", func(n *Notify) { n.Ntfy.Reply.Token = "" }, true},
		{"missing topic rejected", func(n *Notify) { n.Ntfy.Reply.Topic = "" }, true},
		{"topic must differ from notify topic", func(n *Notify) { n.Ntfy.Reply.Topic = "notify-topic" }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := base()
			tc.mutate(&n)
			err := ValidateReply(n)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateReply(%+v) = nil, want error", n.Ntfy.Reply)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateReply(%+v) = %v, want nil", n.Ntfy.Reply, err)
			}
		})
	}
}

func TestValidateReply_ConversationBackendsFailClosed(t *testing.T) {
	validSlack := Notify{Slack: NotifySlack{
		Enabled: true, BotToken: "xoxb-test", SigningSecret: "signing", AllowedUsers: []string{"U1"}, AllowedChats: []string{"D1"},
	}}
	if err := ValidateReply(validSlack); err != nil {
		t.Fatalf("valid Slack config: %v", err)
	}
	for _, mutate := range []func(*Notify){
		func(n *Notify) { n.Slack.BotToken = "" },
		func(n *Notify) { n.Slack.SigningSecret = "" },
		func(n *Notify) { n.Slack.AllowedUsers = nil },
		func(n *Notify) { n.Slack.AllowedChats = nil },
	} {
		n := validSlack
		mutate(&n)
		if err := ValidateReply(n); err == nil {
			t.Fatal("incomplete enabled Slack config was accepted")
		}
	}

	validTelegram := Notify{Telegram: NotifyTelegram{
		Enabled: true, BotToken: "123:token", AllowedUsers: []string{"1"}, AllowedChats: []string{"2"},
	}}
	if err := ValidateReply(validTelegram); err != nil {
		t.Fatalf("valid Telegram config: %v", err)
	}
	for _, mutate := range []func(*Notify){
		func(n *Notify) { n.Telegram.BotToken = "" },
		func(n *Notify) { n.Telegram.AllowedUsers = nil },
		func(n *Notify) { n.Telegram.AllowedChats = nil },
	} {
		n := validTelegram
		mutate(&n)
		if err := ValidateReply(n); err == nil {
			t.Fatal("incomplete enabled Telegram config was accepted")
		}
	}
}
