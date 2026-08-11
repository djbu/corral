package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/djbu/corral/internal/config"
)

func TestBuildNotifyBackends_ConversationChannels(t *testing.T) {
	cfg := config.Notify{
		Enabled: true,
		Timeout: time.Second,
		Slack: config.NotifySlack{
			Enabled: true, BotToken: "slack-secret", SigningSecret: "signing", AllowedUsers: []string{"U1"}, AllowedChats: []string{"D1", "D2"},
		},
		Telegram: config.NotifyTelegram{
			Enabled: true, BotToken: "telegram-secret", AllowedUsers: []string{"1"}, AllowedChats: []string{"2"},
		},
	}
	if got, want := backendNames(buildNotifyBackends(cfg)), []string{"slack", "slack", "telegram"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backendNames = %v, want %v", got, want)
	}
}

func TestBuildNotifyBackends_GlobalDisableWins(t *testing.T) {
	cfg := config.Notify{Slack: config.NotifySlack{Enabled: true, AllowedChats: []string{"D1"}}}
	if got := buildNotifyBackends(cfg); len(got) != 0 {
		t.Fatalf("disabled notify built %d backends", len(got))
	}
}
