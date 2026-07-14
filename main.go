// Command tg-claude-bot is a Telegram bot that answers messages by driving
// the Claude Code CLI in print mode, authenticated with a Claude subscription
// (OAuth) instead of an API key. It uses long-polling (getUpdates), so it
// works behind NAT without a public IP, and it depends only on the Go
// standard library plus the `claude` binary.
//
// Each chat (group or private) gets its own persistent Claude Code session,
// so conversations have memory. Sessions idle for more than an hour are
// expired and their transcripts deleted; the next message starts fresh.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

const (
	// telegramMsgLimit is Telegram's maximum message length. We split well
	// below it to leave margin for the HTML tags and escapes the Markdown
	// conversion adds.
	telegramMsgLimit = 3500

	// telegramStylePrompt keeps Claude's Markdown inside the subset this bot
	// converts to Telegram HTML; it is always appended to the system prompt.
	telegramStylePrompt = "Your replies are shown in Telegram, which renders only limited formatting. Use only: **bold**, `inline code`, fenced ``` code blocks, and [links](https://example.com). Never use headers, tables, italics, or nested lists — write plain paragraphs and simple '-' bullet lists instead. Keep replies concise; this is a chat app."

	// maxReplyParts caps how many Telegram messages one answer may span, so
	// a prompt that coaxes Claude into a huge dump can't flood the chat.
	maxReplyParts = 4

	// blockedReplyCooldown is the initial interval between access-denied
	// replies in one chat; it doubles per reply up to blockedReplyMax, so a
	// legit user discovers their ID instantly while a hostile group the bot
	// was dumped into decays to a few replies a day.
	blockedReplyCooldown = time.Minute
	blockedReplyMax      = 6 * time.Hour

	// pollTimeout is the getUpdates long-poll duration in seconds.
	pollTimeout = 50

	// maxConcurrent bounds the number of claude processes running at once.
	// Each one is a full Claude Code instance, so keep this modest.
	maxConcurrent = 4

	// defaultIdleTimeout is how long a chat's session may sit unused before
	// the janitor expires it and deletes its transcript (override with
	// SESSION_IDLE_MINUTES).
	defaultIdleTimeout = time.Hour

	// defaultMaxAge caps a session's total lifetime: even a chat that never
	// goes idle starts a fresh session (no previous context) once a week
	// (override with SESSION_MAX_AGE_DAYS).
	defaultMaxAge = 7 * 24 * time.Hour

	// claudeTimeout bounds one claude CLI invocation (a single reply,
	// possibly including web searches).
	claudeTimeout = 5 * time.Minute

	// defaultReaction is the emoji reaction set on a message the bot is
	// about to answer, as an immediate "seen, working on it" acknowledgment.
	// Telegram only accepts emoji from its fixed reaction set; override
	// with REACTION_EMOJI (set it to "none" to disable).
	defaultReaction = "👀"

	// claudeTools is the only tool surface exposed to the model. Telegram
	// messages are untrusted input, so no Bash, no file tools, no MCP —
	// and no WebFetch, which would fetch attacker-chosen URLs from inside
	// our network (SSRF); WebSearch runs server-side at Anthropic.
	claudeTools = "WebSearch"
)

type config struct {
	telegramToken string
	apiBase       string // Telegram Bot API base URL, for self-hosted Bot API servers
	oauthToken    string // CLAUDE_CODE_OAUTH_TOKEN; may be empty if `claude` is already logged in
	model         string // optional --model value (alias like "sonnet" or a full model ID)
	systemPrompt  string
	claudeBin     string
	idleTimeout   time.Duration
	maxAge        time.Duration
	allowedUsers  allowList
	allowedChats  allowList
	allowAll      bool   // explicit ALLOW_ALL=true opt-out of access control
	reactionEmoji string // reaction set on messages the bot is about to answer; "" = disabled
}

func loadConfig() (config, error) {
	cfg := config{
		telegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		apiBase:       os.Getenv("TELEGRAM_API_BASE"),
		oauthToken:    os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"),
		model:         os.Getenv("CLAUDE_MODEL"),
		systemPrompt:  os.Getenv("SYSTEM_PROMPT"),
		claudeBin:     os.Getenv("CLAUDE_BIN"),
		idleTimeout:   defaultIdleTimeout,
		maxAge:        defaultMaxAge,
		reactionEmoji: defaultReaction,
	}
	// Empty keeps the default (docker env_file passes unset vars as empty);
	// only the explicit "none" disables the acknowledgment reaction.
	if v := os.Getenv("REACTION_EMOJI"); v != "" {
		if strings.EqualFold(v, "none") {
			v = ""
		}
		cfg.reactionEmoji = v
	}
	if v := os.Getenv("ALLOW_ALL"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("ALLOW_ALL must be a boolean, got %q", v)
		}
		cfg.allowAll = b
	}
	if cfg.telegramToken == "" {
		return cfg, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.apiBase == "" {
		cfg.apiBase = "https://api.telegram.org"
	}
	cfg.apiBase = strings.TrimSuffix(cfg.apiBase, "/")
	if cfg.claudeBin == "" {
		cfg.claudeBin = "claude"
	}
	if err := envDuration("SESSION_IDLE_MINUTES", time.Minute, &cfg.idleTimeout); err != nil {
		return cfg, err
	}
	if err := envDuration("SESSION_MAX_AGE_DAYS", 24*time.Hour, &cfg.maxAge); err != nil {
		return cfg, err
	}
	var err error
	if cfg.allowedUsers, err = parseAllowList("ALLOWED_USERS", false); err != nil {
		return cfg, err
	}
	if cfg.allowedChats, err = parseAllowList("ALLOWED_CHATS", true); err != nil {
		return cfg, err
	}
	// Fail closed: the bot drives Claude on the operator's account, so an
	// unset or lost allowlist must not silently open it to everyone.
	if cfg.allowedUsers.empty() && cfg.allowedChats.empty() && !cfg.allowAll {
		return cfg, errors.New("no access control configured: set ALLOWED_USERS and/or ALLOWED_CHATS (comma-separated IDs or @usernames), or set ALLOW_ALL=true to explicitly allow everyone")
	}
	return cfg, nil
}

// allowList matches Telegram users or chats by numeric ID (stable, the
// recommended form) or by @username (changeable, a convenience).
type allowList struct {
	ids   map[int64]bool
	names map[string]bool // lowercase, without the leading @
}

func (a allowList) size() int {
	return len(a.ids) + len(a.names)
}

func (a allowList) empty() bool {
	return a.size() == 0
}

// match reports whether the given ID or username (may be empty) is listed.
func (a allowList) match(id int64, username string) bool {
	if a.ids[id] {
		return true
	}
	return username != "" && a.names[strings.ToLower(username)]
}

// parseAllowList reads a comma-separated env var whose entries are numeric
// Telegram IDs or @usernames. wantNegative distinguishes chat lists (group
// and channel IDs are negative) from user lists (user IDs are positive), so
// an ID pasted into the wrong variable fails at startup instead of silently
// never matching.
func parseAllowList(name string, wantNegative bool) (allowList, error) {
	a := allowList{ids: make(map[int64]bool), names: make(map[string]bool)}
	for _, entry := range strings.Split(os.Getenv(name), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(entry, "@"); ok {
			if rest == "" {
				return a, fmt.Errorf("%s: empty @username entry", name)
			}
			a.names[strings.ToLower(rest)] = true
			continue
		}
		id, err := strconv.ParseInt(entry, 10, 64)
		if err != nil {
			return a, fmt.Errorf("%s: entry %q is neither a numeric ID nor an @username", name, entry)
		}
		if wantNegative && id >= 0 {
			return a, fmt.Errorf("%s: entry %q looks like a user ID (group/channel IDs are negative)", name, entry)
		}
		if !wantNegative && id <= 0 {
			return a, fmt.Errorf("%s: entry %q looks like a chat ID (user IDs are positive)", name, entry)
		}
		a.ids[id] = true
	}
	return a, nil
}

// envDuration overwrites *dst with the named env var (a positive integer)
// times unit, leaving *dst untouched when the variable is unset.
func envDuration(name string, unit time.Duration, dst *time.Duration) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fmt.Errorf("%s must be a positive integer, got %q", name, v)
	}
	*dst = time.Duration(n) * unit
	return nil
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
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type tgChat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Username string `json:"username"`
}

type tgEntity struct {
	Type   string  `json:"type"`
	Offset int     `json:"offset"`
	Length int     `json:"length"`
	User   *tgUser `json:"user"`
}

// session is one chat's Claude Code session. Its mutex serializes claude
// invocations for the chat (concurrent --resume of one session would race)
// and guards the other fields.
type session struct {
	mu       sync.Mutex
	id       string // session UUID passed to --session-id / --resume
	workDir  string // empty per-session temp dir claude runs in; removed on expiry
	started  bool   // true once a claude run has succeeded, i.e. --resume is valid
	expired  bool   // removed from the map; holders must discard and re-acquire
	failures int    // consecutive failed claude runs; the session resets after two
	created  time.Time
	lastUsed time.Time
}

// sessionManager maps chat IDs to live sessions.
type sessionManager struct {
	mu          sync.Mutex
	sessions    map[int64]*session
	idleTimeout time.Duration
	maxAge      time.Duration

	// cleanup is called (in its own goroutine) with each expired session
	// after it has been removed from the map, to delete its on-disk state.
	cleanup func(*session)
}

func newSessionManager(idleTimeout, maxAge time.Duration) *sessionManager {
	return &sessionManager{
		sessions:    make(map[int64]*session),
		idleTimeout: idleTimeout,
		maxAge:      maxAge,
	}
}

// staleReason reports why the session should be expired, or "" if it is
// still fresh. The caller must hold s.mu.
func (sm *sessionManager) staleReason(s *session) string {
	switch {
	case time.Since(s.lastUsed) > sm.idleTimeout:
		return fmt.Sprintf("idle > %v", sm.idleTimeout)
	case time.Since(s.created) > sm.maxAge:
		return fmt.Sprintf("age > %v", sm.maxAge)
	}
	return ""
}

// expire marks the session expired, removes it from the map, and schedules
// its disk cleanup. The caller must hold s.mu.
func (sm *sessionManager) expire(chatID int64, s *session, reason string) {
	s.expired = true
	sm.mu.Lock()
	if sm.sessions[chatID] == s {
		delete(sm.sessions, chatID)
	}
	sm.mu.Unlock()
	log.Printf("session %s expired for chat %d (%s)", s.id, chatID, reason)
	if sm.cleanup != nil {
		go sm.cleanup(s)
	}
}

// acquire returns the chat's session with its mutex held, creating a new one
// if none exists or the mapped one is expired or stale. Checking staleness
// here (not just in the janitor) guarantees a chat busy enough to dodge every
// janitor sweep still rotates once it exceeds the max age. The caller must
// call release when done.
func (sm *sessionManager) acquire(chatID int64) *session {
	for {
		sm.mu.Lock()
		s := sm.sessions[chatID]
		if s == nil {
			now := time.Now()
			s = &session{id: newUUID(), created: now, lastUsed: now}
			sm.sessions[chatID] = s
			log.Printf("session %s created for chat %d", s.id, chatID)
		}
		sm.mu.Unlock()

		s.mu.Lock()
		if s.expired {
			s.mu.Unlock()
			continue
		}
		if reason := sm.staleReason(s); reason != "" {
			sm.expire(chatID, s, reason)
			s.mu.Unlock()
			continue
		}
		return s
	}
}

func (sm *sessionManager) release(s *session) {
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// expireStale expires sessions that are past the idle timeout or max age.
// Sessions currently mid-conversation hold their mutex and are skipped via
// TryLock until the next sweep (or until acquire catches them).
func (sm *sessionManager) expireStale() {
	sm.mu.Lock()
	candidates := make(map[int64]*session, len(sm.sessions))
	for chatID, s := range sm.sessions {
		candidates[chatID] = s
	}
	sm.mu.Unlock()

	for chatID, s := range candidates {
		if !s.mu.TryLock() {
			continue
		}
		if !s.expired {
			if reason := sm.staleReason(s); reason != "" {
				sm.expire(chatID, s, reason)
			}
		}
		s.mu.Unlock()
	}
}

// newUUID returns a random RFC 4122 v4 UUID; claude --session-id requires
// this format.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable and can't produce a usable ID.
		log.Fatalf("crypto/rand: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type bot struct {
	cfg         config
	botID       int64
	botUsername string
	workBase    string
	sessions    *sessionManager

	// claudeEnv is the child environment for claude runs, with
	// ANTHROPIC_API_KEY dropped so replies always use subscription auth.
	claudeEnv []string

	// sem bounds concurrent claude processes. It is taken only around the
	// claude run itself, never while waiting for a chat's session mutex, so
	// a burst of messages in one chat can't starve every other chat.
	sem chan struct{}

	// redactor strips secrets from anything we log.
	redactor *strings.Replacer

	// blocked rate-limits the access-denied replies per chat.
	blockedMu sync.Mutex
	blocked   map[int64]*blockedState

	// reactionOK flips off permanently if Telegram rejects the configured
	// reaction emoji, so a bad REACTION_EMOJI fails once, not per message.
	reactionOK atomic.Bool

	// tgClient's timeout must exceed pollTimeout or long polls would be
	// cancelled by our own client.
	tgClient *http.Client
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if _, err := exec.LookPath(cfg.claudeBin); err != nil {
		log.Fatalf("claude CLI not found (%v); install it and either run `claude login` or set CLAUDE_CODE_OAUTH_TOKEN from `claude setup-token`", err)
	}
	if cfg.oauthToken == "" {
		log.Printf("CLAUDE_CODE_OAUTH_TOKEN not set; relying on existing `claude` login state")
	}

	// Each session gets its own empty temp directory under workBase as
	// claude's cwd. Sessions don't survive a restart, so leftovers from a
	// previous run are cleared on startup.
	workBase := filepath.Join(os.TempDir(), "tg-claude-bot")
	if err := os.RemoveAll(workBase); err != nil {
		log.Fatalf("workdir: %v", err)
	}
	if err := os.MkdirAll(workBase, 0o700); err != nil {
		log.Fatalf("workdir: %v", err)
	}

	redactPairs := []string{cfg.telegramToken, "[REDACTED]"}
	if cfg.oauthToken != "" {
		redactPairs = append(redactPairs, cfg.oauthToken, "[REDACTED]")
	}

	b := &bot{
		cfg:      cfg,
		workBase: workBase,
		sessions: newSessionManager(cfg.idleTimeout, cfg.maxAge),
		claudeEnv: slices.DeleteFunc(os.Environ(), func(kv string) bool {
			return strings.HasPrefix(kv, "ANTHROPIC_API_KEY=")
		}),
		sem:      make(chan struct{}, maxConcurrent),
		redactor: strings.NewReplacer(redactPairs...),
		blocked:  make(map[int64]*blockedState),
		tgClient: &http.Client{Timeout: (pollTimeout + 20) * time.Second},
	}
	b.reactionOK.Store(true)
	b.sessions.cleanup = b.cleanupSession
	b.checkClaudeAuth()
	b.cleanupOrphanedProjects()

	me, err := b.getMe()
	if err != nil {
		log.Fatalf("getMe: %v", b.redact(err))
	}
	b.botID = me.ID
	b.botUsername = me.Username
	access := fmt.Sprintf("%d allowed user(s), %d allowed chat(s)",
		cfg.allowedUsers.size(), cfg.allowedChats.size())
	if cfg.allowAll {
		access = "ALLOW_ALL — open to everyone"
	}
	log.Printf("started as @%s (model %q, session idle timeout %v, max age %v, access: %s)", b.botUsername, cfg.model, cfg.idleTimeout, cfg.maxAge, access)

	go b.janitorLoop()
	b.pollLoop()
}

// janitorLoop periodically expires stale sessions (disk cleanup happens via
// the session manager's cleanup callback). The check interval scales with
// the idle timeout so short timeouts still expire promptly.
func (b *bot) janitorLoop() {
	interval := max(min(b.cfg.idleTimeout/4, time.Minute), 10*time.Second)
	for range time.Tick(interval) {
		b.sessions.expireStale()
	}
}

// cleanupSession removes an expired session's on-disk state: its transcript
// and its temp working directory.
func (b *bot) cleanupSession(s *session) {
	b.deleteTranscripts(s.id)
	if s.workDir != "" {
		if err := os.RemoveAll(s.workDir); err != nil {
			log.Printf("delete workdir %s: %v", s.workDir, err)
		}
	}
}

// checkClaudeAuth fails fast at startup when claude has no working
// credentials, instead of surfacing auth errors one user message at a time.
func (b *bot) checkClaudeAuth() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.cfg.claudeBin, "auth", "status", "--json")
	cmd.Env = b.claudeEnv
	out, err := cmd.Output()
	if err != nil {
		// Older CLIs may lack the subcommand; don't block startup on that.
		log.Printf("claude auth status check skipped: %v", b.redact(err))
		return
	}
	var st struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		log.Printf("claude auth status check skipped: unparseable output")
		return
	}
	if !st.LoggedIn {
		log.Fatalf("claude is not logged in: run `claude login`, or set CLAUDE_CODE_OAUTH_TOKEN from `claude setup-token`")
	}
}

// cleanupOrphanedProjects removes transcript project dirs left behind by a
// previous run. Sessions don't survive a restart, so every project dir
// derived from a per-session workdir (…/tg-claude-bot/chat-*) is an orphan.
func (b *bot) cleanupOrphanedProjects() {
	matches, err := filepath.Glob(filepath.Join(claudeConfigDir(), "projects", "*tg-claude-bot-chat-*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			log.Printf("delete orphaned project dir %s: %v", m, err)
		}
	}
	if len(matches) > 0 {
		log.Printf("removed %d orphaned session project dir(s)", len(matches))
	}
}

func claudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// deleteTranscripts removes the session's transcript files under the claude
// config dir so expired conversations don't accumulate on disk. The ID is
// generated by this bot (never user input), so the glob is safe.
func (b *bot) deleteTranscripts(sessionID string) {
	configDir := claudeConfigDir()
	if configDir == "" {
		return
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			log.Printf("delete transcript %s: %v", m, err)
			continue
		}
		// The per-session cwd gives each session its own project dir; once
		// it holds no other transcripts, remove it (and side files like
		// memory/) entirely.
		dir := filepath.Dir(m)
		if left, err := filepath.Glob(filepath.Join(dir, "*.jsonl")); err == nil && len(left) == 0 {
			_ = os.RemoveAll(dir)
		}
	}
}

// pollLoop fetches updates forever and dispatches each eligible message to a
// worker goroutine. It is the only writer of the offset, so there is no race
// on it. Claude concurrency is bounded inside handleMessage (b.sem), not
// here, so the poll loop never stalls behind a busy chat.
func (b *bot) pollLoop() {
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
			if !b.allowed(msg) {
				// Checking after promptFor means only messages the bot
				// would have answered are handled here — not bystander
				// group chatter.
				b.noteBlocked(msg)
				continue
			}
			go b.handleMessage(msg, prompt)
		}
	}
}

// blockedState is one chat's access-denied reply throttle: no reply before
// until, and the next quiet period doubles per reply up to blockedReplyMax.
type blockedState struct {
	until   time.Time
	backoff time.Duration
}

// noteBlocked logs a blocked message and, unless the chat is in cooldown,
// replies with the IDs needed to get allowlisted. Replies are rate-limited
// per chat with exponential backoff so strangers can't use the bot as a
// spam machine (or make it flood a group it was dumped into); the check is
// synchronous so a flood costs no goroutines.
func (b *bot) noteBlocked(msg *tgMessage) {
	log.Printf("blocked message from user %s in chat %s",
		idLabel(msg.From.ID, msg.From.Username), idLabel(msg.Chat.ID, msg.Chat.Username))

	now := time.Now()
	b.blockedMu.Lock()
	st := b.blocked[msg.Chat.ID]
	if st != nil && now.Before(st.until) {
		b.blockedMu.Unlock()
		return
	}
	// Bound the map by pruning expired entries; live cooldowns are kept, so
	// an attacker filling the map from many chats can't reset them.
	if len(b.blocked) >= 1024 {
		for id, s := range b.blocked {
			if now.After(s.until) {
				delete(b.blocked, id)
			}
		}
	}
	if st == nil {
		st = &blockedState{backoff: blockedReplyCooldown}
		b.blocked[msg.Chat.ID] = st
	}
	st.until = now.Add(st.backoff)
	st.backoff = min(st.backoff*2, blockedReplyMax)
	b.blockedMu.Unlock()

	go b.replyBlocked(msg)
}

// replyBlocked sends the access-denied reply; the sender's own IDs are the
// only detail included (naming config variables would fingerprint the bot
// for anyone probing it).
func (b *bot) replyBlocked(msg *tgMessage) {
	text := fmt.Sprintf("Sorry, you're not on this bot's allowlist. Your user ID: %d.", msg.From.ID)
	if msg.Chat.Type != "private" {
		text += fmt.Sprintf(" This chat's ID: %d.", msg.Chat.ID)
	}
	text += " Ask the bot's owner to allowlist you."

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.send(ctx, msg.Chat.ID, text, "", msg.MessageID); err != nil {
		log.Printf("blocked notice (chat %d): %v", msg.Chat.ID, b.redact(err))
		// Don't burn the cooldown on a failed send — let a retry through.
		b.blockedMu.Lock()
		delete(b.blocked, msg.Chat.ID)
		b.blockedMu.Unlock()
	}
}

// react sets the acknowledgment emoji reaction on a message the bot is about
// to answer, so the sender sees it was picked up before Claude finishes.
// Best-effort: failures (e.g. an emoji outside Telegram's reaction set, or a
// group where reactions are restricted) are logged and otherwise ignored.
func (b *bot) react(msg *tgMessage) {
	if !b.reactionOK.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := b.tgCall(ctx, "setMessageReaction", map[string]any{
		"chat_id":    msg.Chat.ID,
		"message_id": msg.MessageID,
		"reaction":   []map[string]any{{"type": "emoji", "emoji": b.cfg.reactionEmoji}},
	}, nil)
	if err != nil {
		log.Printf("setMessageReaction (chat %d): %v", msg.Chat.ID, b.redact(err))
		// A 400 means the configured emoji isn't in Telegram's reaction
		// set; stop trying rather than fail on every message. Other errors
		// (chat-level restrictions, transient) don't disable it globally.
		if strings.Contains(err.Error(), "status 400") && strings.Contains(err.Error(), "REACTION_INVALID") {
			log.Printf("REACTION_EMOJI %q rejected by Telegram; disabling reactions", b.cfg.reactionEmoji)
			b.reactionOK.Store(false)
		}
	}
}

// idLabel formats a Telegram ID with its username, if there is one, for the
// blocked-message log the operator uses to discover IDs to allowlist.
func idLabel(id int64, username string) string {
	if username == "" {
		return strconv.FormatInt(id, 10)
	}
	return fmt.Sprintf("%d (@%s)", id, username)
}

// allowed reports whether the message passes access control: the group or
// channel is allowlisted (anyone in an allowed chat may use the bot there)
// or the sender is allowlisted (an allowed user may use the bot anywhere,
// including private chat). ALLOWED_CHATS is deliberately not consulted for
// private chats — there chat ID and username mirror the user's, which would
// let a chat entry silently double as a personal grant. ALLOW_ALL=true
// disables the check entirely.
func (b *bot) allowed(msg *tgMessage) bool {
	if b.cfg.allowAll {
		return true
	}
	if msg.Chat.Type != "private" && b.cfg.allowedChats.match(msg.Chat.ID, msg.Chat.Username) {
		return true
	}
	return b.cfg.allowedUsers.match(msg.From.ID, msg.From.Username)
}

// promptFor decides whether the bot should answer msg and returns the text to
// send to Claude. Private chats always get a reply. In groups the bot replies
// only when @mentioned (mention stripped from the prompt) or when the message
// is a reply to one of the bot's own messages. Group prompts are prefixed
// with the sender's name so the shared session can tell speakers apart.
func (b *bot) promptFor(msg *tgMessage) (string, bool) {
	switch msg.Chat.Type {
	case "private":
		return msg.Text, true
	case "group", "supergroup":
		if prompt, ok := b.stripMention(msg.Text, msg.Entities); ok {
			return groupPrompt(msg.From, prompt), true
		}
		if r := msg.ReplyToMessage; r != nil && r.From != nil && r.From.ID == b.botID {
			return groupPrompt(msg.From, msg.Text), true
		}
	}
	return "", false
}

// groupPrompt labels a group message with its sender. The label is plain
// data inside the prompt; Claude uses it to distinguish participants in the
// shared per-group session.
func groupPrompt(from *tgUser, text string) string {
	name := from.FirstName
	if from.Username != "" {
		name = "@" + from.Username
	}
	if name == "" {
		return text
	}
	return fmt.Sprintf("[%s] %s", name, text)
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

// handleMessage asks the chat's Claude Code session for an answer and sends
// it back, showing a "typing" chat action while waiting. Waiting for the
// chat's session mutex and for a claude slot has no deadline — the claude
// run itself is bounded inside askClaude, and reply delivery gets its own
// context so a slow turn can't eat the send budget.
func (b *bot) handleMessage(msg *tgMessage, prompt string) {
	if b.cfg.reactionEmoji != "" {
		go b.react(msg)
	}
	typingCtx, stopTypingCtx := context.WithCancel(context.Background())
	stopTyping := b.typeWhile(typingCtx, msg.Chat.ID)

	s := b.sessions.acquire(msg.Chat.ID)
	b.sem <- struct{}{}
	answer, err := b.askClaude(s, prompt)
	<-b.sem
	b.sessions.release(s)

	stopTyping()
	stopTypingCtx()

	if err != nil {
		log.Printf("claude (chat %d): %v", msg.Chat.ID, b.redact(err))
		answer = "Sorry, I couldn't process that message right now. Please try again later."
	}

	parts := splitMessage(answer, telegramMsgLimit)
	if len(parts) > maxReplyParts {
		log.Printf("reply for chat %d truncated from %d to %d parts", msg.Chat.ID, len(parts), maxReplyParts)
		parts = parts[:maxReplyParts]
		parts[maxReplyParts-1] += "\n[reply truncated]"
	}
	balanceFences(parts)

	sendCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i, part := range parts {
		// Reply to the original message with the first chunk so the answer is
		// attributable in group chats; follow-up chunks are sent plain.
		var replyTo int64
		if i == 0 {
			replyTo = msg.MessageID
		}
		// Claude writes Markdown; Telegram renders HTML. Convert, and fall
		// back to the raw text on any 400 — bad entities, or a message the
		// added tags/escapes pushed over the length limit.
		err := b.send(sendCtx, msg.Chat.ID, mdToHTML(part), "HTML", replyTo)
		if err != nil && strings.Contains(err.Error(), "status 400") {
			err = b.send(sendCtx, msg.Chat.ID, part, "", replyTo)
		}
		if err != nil {
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

// askClaude runs one turn of the chat's Claude Code session. The caller must
// hold s.mu (acquire does this). The prompt is passed on stdin so message
// text can never be parsed as CLI flags.
func (b *bot) askClaude(s *session, prompt string) (string, error) {
	runCtx, cancel := context.WithTimeout(context.Background(), claudeTimeout)
	defer cancel()

	if s.workDir == "" {
		dir, err := os.MkdirTemp(b.workBase, "chat-")
		if err != nil {
			return "", fmt.Errorf("create session workdir: %w", err)
		}
		s.workDir = dir
	}

	args := []string{
		"-p",
		"--output-format", "text",
		"--tools", claudeTools,
		"--allowedTools", claudeTools,
		"--strict-mcp-config",
	}
	if s.started {
		args = append(args, "--resume", s.id)
	} else {
		args = append(args, "--session-id", s.id)
	}
	if b.cfg.model != "" {
		args = append(args, "--model", b.cfg.model)
	}
	sysPrompt := telegramStylePrompt
	if b.cfg.systemPrompt != "" {
		sysPrompt += "\n\n" + b.cfg.systemPrompt
	}
	args = append(args, "--append-system-prompt", sysPrompt)

	cmd := exec.CommandContext(runCtx, b.cfg.claudeBin, args...)
	cmd.Dir = s.workDir
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = b.claudeEnv
	// On timeout only the claude process is killed; if a child it spawned
	// keeps our stdout pipe open, WaitDelay stops Run from hanging forever
	// (which would wedge this chat's session mutex permanently).
	cmd.WaitDelay = 15 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		b.noteFailure(s)
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		// Redact before truncating: cutting first could slice a secret in
		// half so the redactor no longer matches it.
		detail = b.redactor.Replace(detail)
		if r := []rune(detail); len(r) > 500 {
			detail = string(r[:500]) + "…"
		}
		return "", fmt.Errorf("claude: %w: %s", err, detail)
	}

	// The run succeeded, so the session ID is registered with claude and
	// --resume is what's valid from now on — even if the answer is empty.
	s.started = true
	s.failures = 0

	answer := strings.TrimSpace(stdout.String())
	if answer == "" {
		return "", errors.New("claude: empty response")
	}
	return answer, nil
}

// noteFailure records a failed claude run. A failed first run may still have
// registered the session ID, and a session whose --resume fails repeatedly
// (corrupt or missing transcript) would otherwise stay broken until it ages
// out — in both cases start over with a fresh ID and no context.
func (b *bot) noteFailure(s *session) {
	s.failures++
	if !s.started || s.failures >= 2 {
		oldID := s.id
		go b.deleteTranscripts(oldID)
		s.id = newUUID()
		s.started = false
		s.failures = 0
		log.Printf("session %s reset to %s after failed claude run", oldID, s.id)
	}
}

// Inline Markdown patterns converted to Telegram HTML. They run on
// HTML-escaped text, so the replacements cannot collide with user content.
var (
	reInlineCode = regexp.MustCompile("`([^`\n]+)`")
	reBold       = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	reStrike     = regexp.MustCompile(`~~([^~\n]+?)~~`)
	// The URL part tolerates one level of parentheses (Wikipedia-style).
	reLink    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+(?:\([^\s)]*\)[^\s)]*)*)\)`)
	reHeader  = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+)$`)
	reBullet  = regexp.MustCompile(`(?m)^([ \t]*)[-*][ \t]+`)
	reLangTag = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+#.-]*$`)
)

// mdToHTML converts the small Markdown subset Claude is instructed to use
// (telegramStylePrompt) into Telegram HTML: fenced code blocks, inline code,
// bold, strikethrough, links, headers (rendered bold), and bullets.
// Everything else is escaped and rendered literally.
func mdToHTML(text string) string {
	var sb strings.Builder
	for i, seg := range strings.Split(text, "```") {
		if i%2 == 1 {
			// Fenced code block; drop the leading line only when it looks
			// like a language tag, not a first line of code.
			if nl := strings.IndexByte(seg, '\n'); nl >= 0 && reLangTag.MatchString(strings.TrimSpace(seg[:nl])) {
				seg = seg[nl+1:]
			}
			sb.WriteString("<pre>")
			sb.WriteString(html.EscapeString(strings.Trim(seg, "\n")))
			sb.WriteString("</pre>")
			continue
		}
		s := html.EscapeString(seg)
		// Pull inline code out into placeholders first so the other
		// patterns can't rewrite inside <code> spans (Telegram forbids
		// entities nested in code). NUL can't occur in message text.
		var codes []string
		s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
			codes = append(codes, "<code>"+m[1:len(m)-1]+"</code>")
			return fmt.Sprintf("\x00%d\x00", len(codes)-1)
		})
		s = reLink.ReplaceAllString(s, `<a href="$2">$1</a>`)
		s = reBold.ReplaceAllString(s, "<b>$1</b>")
		s = reStrike.ReplaceAllString(s, "<s>$1</s>")
		s = reHeader.ReplaceAllString(s, "<b>$1</b>")
		s = reBullet.ReplaceAllString(s, "$1• ")
		for i, c := range codes {
			s = strings.Replace(s, fmt.Sprintf("\x00%d\x00", i), c, 1)
		}
		sb.WriteString(s)
	}
	return sb.String()
}

// balanceFences repairs fenced code blocks that a chunk split cut in half:
// a chunk that ends inside a block gets a closing fence, and the next chunk
// reopens it, so each chunk converts to valid Telegram HTML on its own.
func balanceFences(parts []string) {
	inside := false
	for i, p := range parts {
		if inside {
			p = "```\n" + p
		}
		endsInside := strings.Count(p, "```")%2 == 1
		if endsInside {
			p += "\n```"
		}
		parts[i] = p
		inside = endsInside
	}
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

func (b *bot) send(ctx context.Context, chatID int64, text, parseMode string, replyTo int64) error {
	params := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if parseMode != "" {
		params["parse_mode"] = parseMode
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

	url := b.cfg.apiBase + "/bot" + b.cfg.telegramToken + "/" + method
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

// redact removes secrets from error text before logging. Transport errors
// (*url.Error) embed the full request URL, which contains the Telegram
// token; claude stderr could conceivably echo the OAuth token.
func (b *bot) redact(err error) string {
	if err == nil {
		return ""
	}
	return b.redactor.Replace(err.Error())
}
