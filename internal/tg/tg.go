// Package tg is a tiny Telegram Bot API client: send messages and long-poll for
// commands. One bot token allows exactly one getUpdates listener, so the whole
// process shares a single Telegram value and a single Listen loop.
package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Telegram struct {
	Token string
	HTTP  *http.Client
}

// SendTo delivers text to a single chat.
func (t *Telegram) SendTo(chatID, text string) {
	body, _ := json.Marshal(map[string]any{
		"chat_id": chatID, "text": text, "disable_web_page_preview": false,
	})
	resp, err := t.HTTP.Post("https://api.telegram.org/bot"+t.Token+"/sendMessage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Println("[tg] send error:", err)
		return
	}
	resp.Body.Close()
}

// SendWebAppButton delivers text with a single inline button that opens the
// Mini App at webURL. Works in private chats.
func (t *Telegram) SendWebAppButton(chatID, text, btnText, webURL string) {
	body, _ := json.Marshal(map[string]any{
		"chat_id": chatID, "text": text, "disable_web_page_preview": true,
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]any{{{
				"text": btnText, "web_app": map[string]string{"url": webURL},
			}}},
		},
	})
	resp, err := t.HTTP.Post("https://api.telegram.org/bot"+t.Token+"/sendMessage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Println("[tg] send error:", err)
		return
	}
	resp.Body.Close()
}

// SetMenuButton makes the bot's default menu button (next to the message input)
// open the Mini App at webURL in every private chat.
func (t *Telegram) SetMenuButton(btnText, webURL string) error {
	body, _ := json.Marshal(map[string]any{
		"menu_button": map[string]any{
			"type": "web_app", "text": btnText,
			"web_app": map[string]string{"url": webURL},
		},
	})
	resp, err := t.HTTP.Post("https://api.telegram.org/bot"+t.Token+"/setChatMenuButton",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if !out.OK {
		return fmt.Errorf("setChatMenuButton: %s", out.Description)
	}
	return nil
}

// Broadcast delivers text to a list of chats.
func (t *Telegram) Broadcast(chatIDs []string, text string) {
	for _, id := range chatIDs {
		t.SendTo(id, text)
	}
}

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

// Listen long-polls for messages from ANY chat, splitting each into a lowercased
// command and its argument string, and dispatches via onCommand until ctx ends.
func (t *Telegram) Listen(ctx context.Context, onCommand func(chatID, cmd, args string)) {
	var offset int64
	client := &http.Client{Timeout: 40 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		u := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?timeout=30&offset=%d", t.Token, offset)
		resp, err := client.Get(u)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		var payload struct {
			OK     bool       `json:"ok"`
			Result []tgUpdate `json:"result"`
		}
		json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		for _, up := range payload.Result {
			offset = up.UpdateID + 1
			if up.Message.Chat.ID == 0 || up.Message.Text == "" {
				continue
			}
			chatID := strconv.FormatInt(up.Message.Chat.ID, 10)
			text := strings.TrimSpace(up.Message.Text)
			cmd, args, _ := strings.Cut(text, " ")
			cmd = strings.ToLower(strings.TrimSpace(cmd))
			if i := strings.IndexByte(cmd, '@'); i > 0 { // strip /cmd@BotName
				cmd = cmd[:i]
			}
			onCommand(chatID, cmd, strings.TrimSpace(args))
		}
	}
}
