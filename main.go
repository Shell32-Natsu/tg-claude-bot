// Command tg-claude-bot is a Telegram bot that answers messages using the
// Anthropic Claude API. It uses long-polling (getUpdates), so it works behind
// NAT without a public IP, and it depends only on the Go standard library.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	defaultModel     = "claude-sonnet-5"

	// maxTokens caps the length (and cost) of a single Claude reply.
	maxTokens = 4096

	// telegramMsgLimit is Telegram's maximum message length. We split a bit
	// below it to leave margin for how Telegram counts characters.
	telegramMsgLimit = 4000

	// pollTimeout is the getUpdates long-poll duration in seconds.
	pollTimeout = 50

	// maxConcurrent bounds the number of messages handled simultaneously.
	maxConcurrent = 8
)

type config struct {
	telegramToken string
	anthropicKey  string
	model         string
	systemPrompt  string
}

func loadConfig() (config, error) {
	cfg := config{
		telegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		anthropicKey:  os.Getenv("ANTHROPIC_API_KEY"),
		model:         os.Getenv("CLAUDE_MODEL"),
		systemPrompt:  os.Getenv("SYSTEM_PROMPT"),
	}
	if cfg.telegramToken == "" {
		return cfg, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.anthropicKey == "" {
		return cfg, errors.New("ANTHROPIC_API_KEY is required")
	}
	if cfg.model == "" {
		cfg.model = defaultModel
	}
	return cfg, nil
}

// Telegram API types (only the fields this bot needs).

type tgResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	MessageID      int64      `json:"message_id"`
	From           *tgUser    `json:"from"`
	Chat           tgChat     `json:"chat"`
	Text           string     `json:"text"`
	Entities       []tgEntity `json:"entities"`
	ReplyToMessage *tgMessage `json:"reply_to_message"`
}

type tgUser struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgEntity struct {
	Type   string  `json:"type"`
	Offset int     `json:"offset"`
	Length int     `json:"length"`
	User   *tgUser `json:"user"`
}

// Anthropic API types.

type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResponse struct {
	Type    string `json:"type"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type bot struct {
	cfg         config
	botID       int64
	botUsername string

	// tgClient's timeout must exceed pollTimeout or long polls would be
	// cancelled by our own client.
	tgClient     *http.Client
	claudeClient *http.Client
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	b := &bot{
		cfg:          cfg,
		tgClient:     &http.Client{Timeout: (pollTimeout + 20) * time.Second},
		claudeClient: &http.Client{Timeout: 3 * time.Minute},
	}

	me, err := b.getMe()
	if err != nil {
		log.Fatalf("getMe: %v", b.redact(err))
	}
	b.botID = me.ID
	b.botUsername = me.Username
	log.Printf("started as @%s (model %s)", b.botUsername, cfg.model)

	b.pollLoop()
}

// pollLoop fetches updates forever and dispatches each eligible message to a
// worker goroutine. It is the only writer of the offset, so there is no race
// on it; concurrency is bounded by the semaphore channel.
func (b *bot) pollLoop() {
	sem := make(chan struct{}, maxConcurrent)
	var offset int64

	for {
		updates, err := b.getUpdates(offset)
		if err != nil {
			log.Printf("getUpdates: %v", b.redact(err))
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			msg := u.Message
			if msg == nil || msg.From == nil || msg.From.IsBot || msg.Text == "" {
				continue
			}
			prompt, ok := b.promptFor(msg)
			if !ok || strings.TrimSpace(prompt) == "" {
				continue
			}
			sem <- struct{}{}
			go func(m *tgMessage, prompt string) {
				defer func() { <-sem }()
				b.handleMessage(m, prompt)
			}(msg, prompt)
		}
	}
}

// promptFor decides whether the bot should answer msg and returns the text to
// send to Claude. Private chats always get a reply. In groups the bot replies
// only when @mentioned (mention stripped from the prompt) or when the message
// is a reply to one of the bot's own messages.
func (b *bot) promptFor(msg *tgMessage) (string, bool) {
	switch msg.Chat.Type {
	case "private":
		return msg.Text, true
	case "group", "supergroup":
		if prompt, ok := b.stripMention(msg.Text, msg.Entities); ok {
			return prompt, true
		}
		if r := msg.ReplyToMessage; r != nil && r.From != nil && r.From.ID == b.botID {
			return msg.Text, true
		}
	}
	return "", false
}

// stripMention looks for a mention of the bot among the message entities and,
// if found, returns the message text with that mention removed. Telegram
// entity offsets and lengths are in UTF-16 code units, so the text is
// converted to UTF-16 before slicing.
func (b *bot) stripMention(text string, entities []tgEntity) (string, bool) {
	u16 := utf16.Encode([]rune(text))
	for _, e := range entities {
		if e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > len(u16) {
			continue
		}
		var mentioned bool
		switch e.Type {
		case "mention":
			ent := string(utf16.Decode(u16[e.Offset : e.Offset+e.Length]))
			mentioned = strings.EqualFold(ent, "@"+b.botUsername)
		case "text_mention":
			mentioned = e.User != nil && e.User.ID == b.botID
		}
		if mentioned {
			rest := make([]uint16, 0, len(u16)-e.Length)
			rest = append(rest, u16[:e.Offset]...)
			rest = append(rest, u16[e.Offset+e.Length:]...)
			return strings.TrimSpace(string(utf16.Decode(rest))), true
		}
	}
	return "", false
}

// handleMessage asks Claude for an answer and sends it back, showing a
// "typing" chat action while waiting.
func (b *bot) handleMessage(msg *tgMessage, prompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	stopTyping := b.typeWhile(ctx, msg.Chat.ID)
	answer, err := b.askClaude(ctx, prompt)
	stopTyping()

	if err != nil {
		log.Printf("claude (chat %d): %v", msg.Chat.ID, b.redact(err))
		answer = "Sorry, I couldn't process that message right now. Please try again later."
	}

	for i, part := range splitMessage(answer, telegramMsgLimit) {
		// Reply to the original message with the first chunk so the answer is
		// attributable in group chats; follow-up chunks are sent plain.
		var replyTo int64
		if i == 0 {
			replyTo = msg.MessageID
		}
		if err := b.sendMessage(ctx, msg.Chat.ID, part, replyTo); err != nil {
			log.Printf("sendMessage (chat %d): %v", msg.Chat.ID, b.redact(err))
			return
		}
	}
}

// typeWhile sends the "typing" chat action immediately and then every few
// seconds (Telegram shows it for ~5s) until the returned stop function is
// called or ctx is done.
func (b *bot) typeWhile(ctx context.Context, chatID int64) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			_ = b.tgCall(ctx, "sendChatAction", map[string]any{
				"chat_id": chatID,
				"action":  "typing",
			}, nil)
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { close(done) }
}

// askClaude calls the Anthropic Messages API. 429 and 5xx responses are
// retried with exponential backoff; other errors are returned immediately.
func (b *bot) askClaude(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(claudeRequest{
		Model:     b.cfg.model,
		MaxTokens: maxTokens,
		System:    b.cfg.systemPrompt,
		Messages:  []claudeMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	const attempts = 3
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", b.cfg.anthropicKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		resp, err := b.claudeClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response: %w", err)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("anthropic API status %d", resp.StatusCode)
			if wait := retryAfter(resp.Header.Get("Retry-After")); wait > 0 {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			continue
		}

		var cr claudeResponse
		if err := json.Unmarshal(respBody, &cr); err != nil {
			return "", fmt.Errorf("anthropic API status %d: unparseable response: %w", resp.StatusCode, err)
		}
		if resp.StatusCode != http.StatusOK {
			if cr.Error != nil {
				return "", fmt.Errorf("anthropic API status %d: %s: %s", resp.StatusCode, cr.Error.Type, cr.Error.Message)
			}
			return "", fmt.Errorf("anthropic API status %d", resp.StatusCode)
		}

		var sb strings.Builder
		for _, block := range cr.Content {
			if block.Type == "text" {
				sb.WriteString(block.Text)
			}
		}
		text := strings.TrimSpace(sb.String())
		if text == "" {
			return "", fmt.Errorf("empty response (stop_reason %q)", cr.StopReason)
		}
		return text, nil
	}
	return "", fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}

func retryAfter(header string) time.Duration {
	secs, err := strconv.Atoi(header)
	if err != nil || secs <= 0 || secs > 60 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// splitMessage breaks text into chunks of at most limit runes, preferring to
// break at a newline and then at a space so words and UTF-8 sequences are
// never cut in half.
func splitMessage(text string, limit int) []string {
	runes := []rune(text)
	var parts []string
	for len(runes) > 0 {
		if len(runes) <= limit {
			parts = append(parts, string(runes))
			break
		}
		cut := limit
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i
				break
			}
		}
		if cut == limit {
			for i := limit; i > limit/2; i-- {
				if runes[i-1] == ' ' {
					cut = i
					break
				}
			}
		}
		part := strings.TrimSpace(string(runes[:cut]))
		if part != "" {
			parts = append(parts, part)
		}
		runes = runes[cut:]
	}
	return parts
}

// Telegram API helpers.

func (b *bot) getMe() (*tgUser, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var me tgUser
	if err := b.tgCall(ctx, "getMe", nil, &me); err != nil {
		return nil, err
	}
	if me.Username == "" {
		return nil, errors.New("getMe returned no username")
	}
	return &me, nil
}

func (b *bot) getUpdates(offset int64) ([]tgUpdate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), (pollTimeout+15)*time.Second)
	defer cancel()
	var updates []tgUpdate
	err := b.tgCall(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         pollTimeout,
		"allowed_updates": []string{"message"},
	}, &updates)
	return updates, err
}

func (b *bot) sendMessage(ctx context.Context, chatID int64, text string, replyTo int64) error {
	params := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if replyTo != 0 {
		params["reply_to_message_id"] = replyTo
	}
	err := b.tgCall(ctx, "sendMessage", params, nil)
	if err != nil && replyTo != 0 && strings.Contains(err.Error(), "message to be replied not found") {
		// The original message was deleted; send without the reply reference.
		delete(params, "reply_to_message_id")
		err = b.tgCall(ctx, "sendMessage", params, nil)
	}
	return err
}

// tgCall performs a Telegram Bot API call and decodes the result into out
// (which may be nil). The bot token is part of the URL, so errors from this
// function must be logged through redact.
func (b *bot) tgCall(ctx context.Context, method string, params any, out any) error {
	var body io.Reader
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("%s: marshal params: %w", method, err)
		}
		body = bytes.NewReader(data)
	}

	url := "https://api.telegram.org/bot" + b.cfg.telegramToken + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.tgClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", method, err)
	}
	var tr tgResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return fmt.Errorf("%s: status %d: unparseable response: %w", method, resp.StatusCode, err)
	}
	if !tr.OK {
		return fmt.Errorf("%s: status %d: %s", method, resp.StatusCode, tr.Description)
	}
	if out != nil {
		if err := json.Unmarshal(tr.Result, out); err != nil {
			return fmt.Errorf("%s: decode result: %w", method, err)
		}
	}
	return nil
}

// redact removes the bot token from error text before logging. Transport
// errors (*url.Error) embed the full request URL, which contains the token.
func (b *bot) redact(err error) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), b.cfg.telegramToken, "[REDACTED]")
}
