package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

// Sender delivers one event to one channel. Adding a channel type (email, …) means adding a
// Sender and a case in NewSender, Validate and SecretFields.
type Sender interface {
	Send(ctx context.Context, e Event) error
}

// DefaultTimeout bounds one delivery attempt when NewSender gets a nil client.
const DefaultTimeout = 15 * time.Second

// NewSender validates the channel config and returns its Sender. hc may be nil.
func NewSender(ch Channel, hc *http.Client) (Sender, error) {
	if err := Validate(ch); err != nil {
		return nil, err
	}
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	switch ch.Type {
	case TypeNtfy:
		var c NtfyConfig
		_ = decode(ch.Config, &c)
		if c.Server == "" {
			c.Server = defaultNtfyServer
		}
		return &ntfy{c: c, hc: hc}, nil
	case TypeGotify:
		var c GotifyConfig
		_ = decode(ch.Config, &c)
		return &gotify{c: c, hc: hc}, nil
	case TypeTelegram:
		var c TelegramConfig
		_ = decode(ch.Config, &c)
		if c.APIBase == "" {
			c.APIBase = defaultTelegramAPI
		}
		return &telegram{c: c, hc: hc}, nil
	case TypeWebhook:
		var c WebhookConfig
		_ = decode(ch.Config, &c)
		w := &webhook{c: c, hc: hc}
		if c.Preset == PresetCustom {
			w.tmpl, _ = parseTemplate(c.Template) // Validate already parsed it
		}
		return w, nil
	}
	return nil, fmt.Errorf("unknown channel type %q", ch.Type)
}

// --- ntfy: https://docs.ntfy.sh/publish/ ---

type ntfy struct {
	c  NtfyConfig
	hc *http.Client
}

var ntfyPriority = map[Severity]string{Info: "3", Warning: "4", Error: "5"}
var ntfyTag = map[Severity]string{Info: "information_source", Warning: "warning", Error: "rotating_light"}

func (n *ntfy) Send(ctx context.Context, e Event) error {
	u, err := url.JoinPath(n.c.Server, n.c.Topic)
	if err != nil {
		return errors.New("ntfy: invalid server URL")
	}
	body := e.Message
	if body == "" {
		body = e.Title // ntfy would otherwise send the literal "triggered"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return errors.New("ntfy: cannot build request")
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", headerValue(e.Title))
	req.Header.Set("Priority", ntfyPriority[e.Severity])
	tags := append([]string{ntfyTag[e.Severity]}, n.c.Tags...)
	req.Header.Set("Tags", headerValue(strings.Join(tags, ",")))
	if e.URL != "" {
		req.Header.Set("Click", headerValue(e.URL))
	}
	switch {
	case n.c.Token != "":
		req.Header.Set("Authorization", "Bearer "+n.c.Token)
	case n.c.Username != "":
		req.SetBasicAuth(n.c.Username, n.c.Password)
	}
	return do(n.hc, req, "ntfy", n.c.Token, n.c.Password)
}

// headerValue makes text safe for an HTTP header: one line, and RFC 2047-encoded when it isn't
// plain ASCII (ntfy decodes that, raw UTF-8 in headers is not reliable through proxies).
func headerValue(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return mime.BEncoding.Encode("utf-8", s)
}

// --- Gotify: https://gotify.net/api-docs#/message/createMessage ---

type gotify struct {
	c  GotifyConfig
	hc *http.Client
}

var gotifyPriority = map[Severity]int{Info: 4, Warning: 6, Error: 8}

func (g *gotify) Send(ctx context.Context, e Event) error {
	u, err := url.JoinPath(g.c.Server, "message")
	if err != nil {
		return errors.New("gotify: invalid server URL")
	}
	msg := map[string]any{
		"title":    e.Title,
		"message":  orTitle(e),
		"priority": gotifyPriority[e.Severity],
	}
	if e.URL != "" {
		msg["extras"] = map[string]any{"client::notification": map[string]any{"click": map[string]string{"url": e.URL}}}
	}
	b, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return errors.New("gotify: cannot build request")
	}
	req.Header.Set("Content-Type", "application/json")
	// The header instead of ?token=: query strings end up in access logs and url.Error messages.
	req.Header.Set("X-Gotify-Key", g.c.Token)
	return do(g.hc, req, "gotify", g.c.Token)
}

// Gotify rejects an empty message.
func orTitle(e Event) string {
	if e.Message == "" {
		return e.Title
	}
	return e.Message
}

// --- Telegram: https://core.telegram.org/bots/api#sendmessage ---

type telegram struct {
	c  TelegramConfig
	hc *http.Client
}

// Telegram's limit is 4096 characters after entity parsing; leave room for title and link.
const telegramMaxMessage = 3500

func (t *telegram) Send(ctx context.Context, e Event) error {
	var sb strings.Builder
	sb.WriteString("<b>" + html.EscapeString(e.Title) + "</b>")
	if e.Message != "" {
		sb.WriteString("\n" + html.EscapeString(truncate(e.Message, telegramMaxMessage)))
	}
	if e.URL != "" {
		sb.WriteString("\n<a href=\"" + html.EscapeString(e.URL) + "\">Open in Dockira</a>")
	}
	msg := map[string]any{
		"chat_id":              t.c.ChatID,
		"text":                 sb.String(),
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if t.c.SilentInfo && e.Severity == Info {
		msg["disable_notification"] = true
	}
	if t.c.ThreadID != 0 {
		msg["message_thread_id"] = t.c.ThreadID
	}
	b, _ := json.Marshal(msg)
	u := strings.TrimRight(t.c.APIBase, "/") + "/bot" + t.c.BotToken + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return errors.New("telegram: cannot build request")
	}
	req.Header.Set("Content-Type", "application/json")
	return do(t.hc, req, "telegram", t.c.BotToken)
}

// --- Webhook ---

type webhook struct {
	c    WebhookConfig
	hc   *http.Client
	tmpl *template.Template
}

var discordColor = map[Severity]int{Info: 0x3498db, Warning: 0xf1c40f, Error: 0xe74c3c}

func (w *webhook) Send(ctx context.Context, e Event) error {
	body, err := w.body(e)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	method := strings.ToUpper(w.c.Method)
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, w.c.URL, bytes.NewReader(body))
	if err != nil {
		return errors.New("webhook: cannot build request")
	}
	req.Header.Set("Content-Type", "application/json")
	secrets := []string{w.c.URL}
	for k, v := range w.c.Headers { // after Content-Type, so a custom template can override it
		req.Header.Set(k, v)
		secrets = append(secrets, v)
	}
	return do(w.hc, req, "webhook", secrets...)
}

func (w *webhook) body(e Event) ([]byte, error) {
	switch w.c.Preset {
	case PresetDiscord:
		embed := map[string]any{
			"title":       truncate(e.Title, 256),
			"description": truncate(e.Message, 4096),
			"color":       discordColor[e.Severity],
			"timestamp":   e.Time.UTC().Format(time.RFC3339),
		}
		if e.URL != "" {
			embed["url"] = e.URL
		}
		return json.Marshal(map[string]any{"content": nil, "embeds": []any{embed}})
	case PresetSlack:
		text := "*" + slackEscape(e.Title) + "*"
		if e.Message != "" {
			text += "\n" + slackEscape(e.Message)
		}
		if e.URL != "" {
			text += " <" + slackEscape(e.URL) + "|Open>"
		}
		return json.Marshal(map[string]string{"text": text})
	case PresetCustom:
		var buf bytes.Buffer
		if err := w.tmpl.Execute(&buf, e); err != nil {
			return nil, fmt.Errorf("template: %w", err)
		}
		return buf.Bytes(), nil
	default:
		return json.Marshal(e)
	}
}

// Slack mrkdwn treats &, < and > as control characters.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// truncate cuts s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// --- shared HTTP handling ---

const excerptLen = 200

// do sends req and turns transport errors and non-2xx answers into errors that name the
// status and a short body excerpt, with every secret scrubbed: the Telegram token is part of
// the URL, which url.Error would otherwise print, and servers sometimes echo what they got.
func do(hc *http.Client, req *http.Request, kind string, secrets ...string) error {
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			// Keep the cause (and errors.Is on timeouts/cancellation) but only scheme://host.
			err = &url.Error{Op: ue.Op, URL: req.URL.Scheme + "://" + req.URL.Host, Err: ue.Err}
		}
		if s := scrub(err.Error(), secrets); s != err.Error() {
			err = errors.New(s)
		}
		return fmt.Errorf("%s: %w", kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // lets the connection be reused
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, excerptLen))
	excerpt := strings.Join(strings.Fields(strings.ToValidUTF8(string(b), "")), " ")
	excerpt = scrub(excerpt, secrets)
	if len(excerpt) > excerptLen { // scrubbing can lengthen it
		excerpt = strings.ToValidUTF8(excerpt[:excerptLen], "")
	}
	msg := kind + ": HTTP " + strconv.Itoa(resp.StatusCode)
	if excerpt != "" {
		msg += ": " + excerpt
	}
	return errors.New(msg)
}

func scrub(s string, secrets []string) string {
	for _, sec := range secrets {
		if len(sec) >= 4 { // don't turn every "a" into stars
			s = strings.ReplaceAll(s, sec, "*****")
		}
	}
	return s
}
