package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/djbu/corral/internal/clock"
)

// TelegramSubscriber uses outbound getUpdates polling so a corral daemon does
// not need a public webhook listener. update_id is the provider retry identity
// and Conversation's durable ledger remains the final duplicate defense.
type TelegramSubscriber struct {
	endpoint     string
	conversation *Conversation
	client       *http.Client
	clk          clock.Clock
	log          *slog.Logger
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	offset       int64
}

func NewTelegramSubscriber(token string, conversation *Conversation, clk clock.Clock, log *slog.Logger) *TelegramSubscriber {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &TelegramSubscriber{
		endpoint:     "https://api.telegram.org/bot" + token + "/getUpdates",
		conversation: conversation,
		client:       newReplyClient(),
		clk:          clk,
		log:          log,
		ctx:          ctx,
		cancel:       cancel,
	}
}

func (s *TelegramSubscriber) Start() { s.wg.Add(1); go s.run() }
func (s *TelegramSubscriber) Close() { s.cancel(); s.wg.Wait() }

func (s *TelegramSubscriber) run() {
	defer s.wg.Done()
	for s.ctx.Err() == nil {
		if err := s.poll(); err != nil && s.ctx.Err() == nil {
			s.log.Warn("notify: telegram getUpdates failed", "err", err)
			select {
			case <-s.ctx.Done():
			case <-s.clk.After(time.Second):
			}
		}
	}
}

func (s *TelegramSubscriber) poll() error {
	u, err := url.Parse(s.endpoint)
	if err != nil {
		return fmt.Errorf("telegram: build getUpdates URL: %w", err)
	}
	q := u.Query()
	q.Set("timeout", "30")
	if s.offset > 0 {
		q.Set("offset", strconv.FormatInt(s.offset, 10))
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("telegram: build getUpdates request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: getUpdates: %w", err)
	}
	defer resp.Body.Close()
	if err := classifyStatus("telegram", resp.StatusCode); err != nil {
		return err
	}
	var result struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID int64 `json:"update_id"`
			Message  struct {
				Text string `json:"text"`
				From struct {
					ID json.Number `json:"id"`
				} `json:"from"`
				Chat struct {
					ID json.Number `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
	}
	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("telegram: decode getUpdates: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("telegram: getUpdates API rejected request")
	}
	for _, update := range result.Result {
		if update.UpdateID >= s.offset {
			s.offset = update.UpdateID + 1
		}
		if update.Message.Text == "" || update.Message.From.ID == "" || update.Message.Chat.ID == "" {
			continue
		}
		decision, err := s.conversation.Accept(s.ctx, IncomingMessage{Backend: "telegram", MessageID: strconv.FormatInt(update.UpdateID, 10), SenderID: update.Message.From.ID.String(), ChatID: update.Message.Chat.ID.String(), ReceivedAt: s.clk.Now(), Body: update.Message.Text})
		if err != nil {
			return err
		}
		s.log.Debug("notify: telegram reply processed", "decision", decision)
	}
	return nil
}
