package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"text/template"
)

// Channel is one configured destination. Config is JSON whose shape depends on Type
// (NtfyConfig, GotifyConfig, TelegramConfig, WebhookConfig).
type Channel struct {
	ID      int64           `json:"id"`
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	Enabled bool            `json:"enabled"`
	Config  json.RawMessage `json:"config"`
}

const (
	TypeNtfy     = "ntfy"
	TypeGotify   = "gotify"
	TypeTelegram = "telegram"
	TypeWebhook  = "webhook"
)

type NtfyConfig struct {
	Server   string   `json:"server"` // default https://ntfy.sh
	Topic    string   `json:"topic"`
	Token    string   `json:"token,omitempty"` // access token (Bearer), or…
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"` // …basic auth
	Tags     []string `json:"tags,omitempty"`
}

const defaultNtfyServer = "https://ntfy.sh"

type GotifyConfig struct {
	Server string `json:"server"`
	Token  string `json:"token"` // application token
}

type TelegramConfig struct {
	BotToken   string `json:"bot_token"`
	ChatID     string `json:"chat_id"`               // numeric id or @channelname
	ThreadID   int    `json:"thread_id,omitempty"`   // forum topic
	SilentInfo bool   `json:"silent_info,omitempty"` // deliver info events without sound
	APIBase    string `json:"api_base,omitempty"`    // default https://api.telegram.org (tests, local Bot API servers)
}

const defaultTelegramAPI = "https://api.telegram.org"

type WebhookConfig struct {
	URL      string            `json:"url"`
	Method   string            `json:"method,omitempty"` // POST (default), PUT or PATCH
	Headers  map[string]string `json:"headers,omitempty"`
	Preset   string            `json:"preset,omitempty"`   // "json" (default), "discord", "slack", "custom"
	Template string            `json:"template,omitempty"` // custom: text/template over Event producing the body
}

const (
	PresetJSON    = "json"
	PresetDiscord = "discord"
	PresetSlack   = "slack"
	PresetCustom  = "custom"
)

// Validate checks the fields each channel type needs. Error messages never echo field values:
// several of them (webhook URLs, tokens) are secrets.
func Validate(ch Channel) error {
	var errs []error
	if strings.TrimSpace(ch.Name) == "" {
		errs = append(errs, errors.New("name is required"))
	}
	switch ch.Type {
	case TypeNtfy:
		var c NtfyConfig
		if err := decode(ch.Config, &c); err != nil {
			return err
		}
		errs = append(errs, c.validate()...)
	case TypeGotify:
		var c GotifyConfig
		if err := decode(ch.Config, &c); err != nil {
			return err
		}
		errs = append(errs, checkURL("server", c.Server, true))
		if c.Token == "" {
			errs = append(errs, errors.New("token is required"))
		}
	case TypeTelegram:
		var c TelegramConfig
		if err := decode(ch.Config, &c); err != nil {
			return err
		}
		errs = append(errs, c.validate()...)
	case TypeWebhook:
		var c WebhookConfig
		if err := decode(ch.Config, &c); err != nil {
			return err
		}
		errs = append(errs, c.validate()...)
	default:
		return fmt.Errorf("unknown channel type %q", ch.Type)
	}
	return errors.Join(errs...)
}

func decode(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		// json errors can quote the offending value, which may be a secret.
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return fmt.Errorf("config: field %s has the wrong type", te.Field)
		}
		return errors.New("config is not a valid JSON object")
	}
	return nil
}

// ntfy's own topic rule.
var ntfyTopic = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

func (c NtfyConfig) validate() []error {
	errs := []error{checkURL("server", c.Server, false)}
	if !ntfyTopic.MatchString(c.Topic) {
		errs = append(errs, errors.New("topic is required: 1–64 letters, digits, - or _"))
	}
	if c.Token != "" && (c.Username != "" || c.Password != "") {
		errs = append(errs, errors.New("use either a token or username/password, not both"))
	}
	if (c.Username == "") != (c.Password == "") {
		errs = append(errs, errors.New("username and password must be set together"))
	}
	for _, t := range c.Tags {
		if t == "" || strings.ContainsAny(t, ",\r\n") {
			errs = append(errs, errors.New("tags must be non-empty and contain no commas"))
			break
		}
	}
	return errs
}

func (c TelegramConfig) validate() []error {
	errs := []error{checkURL("api_base", c.APIBase, false)}
	if c.BotToken == "" {
		errs = append(errs, errors.New("bot_token is required"))
	} else if strings.ContainsAny(c.BotToken, "/?#% \r\n") {
		errs = append(errs, errors.New("bot_token contains invalid characters"))
	}
	if c.ChatID == "" {
		errs = append(errs, errors.New("chat_id is required"))
	}
	if c.ThreadID < 0 {
		errs = append(errs, errors.New("thread_id must not be negative"))
	}
	return errs
}

func (c WebhookConfig) validate() []error {
	errs := []error{checkURL("url", c.URL, true)}
	switch strings.ToUpper(c.Method) {
	case "", "POST", "PUT", "PATCH":
	default:
		errs = append(errs, errors.New("method must be POST, PUT or PATCH"))
	}
	for k, v := range c.Headers {
		if !headerName.MatchString(k) {
			errs = append(errs, errors.New("headers: invalid header name"))
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			errs = append(errs, errors.New("headers: values must be a single line"))
		}
	}
	switch c.Preset {
	case "", PresetJSON, PresetDiscord, PresetSlack:
	case PresetCustom:
		if strings.TrimSpace(c.Template) == "" {
			errs = append(errs, errors.New("template is required for the custom preset"))
		} else if _, err := parseTemplate(c.Template); err != nil {
			errs = append(errs, fmt.Errorf("template: %w", err))
		}
	default:
		errs = append(errs, errors.New("preset must be json, discord, slack or custom"))
	}
	return errs
}

// RFC 9110 token characters.
var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// checkURL requires an absolute http(s) URL. It never puts the value in the error: url.Parse
// errors quote the whole URL, and a webhook URL usually contains a token.
func checkURL(field, raw string, required bool) error {
	if raw == "" {
		if required {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an http:// or https:// URL", field)
	}
	return nil
}

func parseTemplate(text string) (*template.Template, error) {
	return template.New("custom").Option("missingkey=zero").Funcs(template.FuncMap{
		// json marshals any value, quotes included: {"text": {{json .Title}}}.
		// No HTML escaping: the receiver is an API, not a web page.
		"json": func(v any) (string, error) {
			var b strings.Builder
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			err := enc.Encode(v)
			return strings.TrimSuffix(b.String(), "\n"), err
		},
	}).Parse(text)
}

// SecretFields lists the config fields of a channel type that are encrypted at rest and never
// returned by the API. Webhook URLs count: Discord and Slack put the token in the URL.
// For webhooks, "headers" means every header value (names are not secret).
func SecretFields(typ string) []string {
	switch typ {
	case TypeNtfy:
		return []string{"token", "password"}
	case TypeGotify:
		return []string{"token"}
	case TypeTelegram:
		return []string{"bot_token"}
	case TypeWebhook:
		return []string{"url", "headers"}
	}
	return nil
}

// Redact returns the channel's config with secret values blanked ("" for strings; header names
// kept with "" values), plus the names of the secret fields that are set, so the UI can show
// "••••• (set)". Unparseable config yields "{}": better to show nothing than to leak.
func Redact(ch Channel) (json.RawMessage, []string) {
	m, err := object(ch.Config)
	if err != nil {
		return json.RawMessage("{}"), nil
	}
	var set []string
	for _, f := range SecretFields(ch.Type) {
		v, ok := m[f]
		if !ok {
			continue
		}
		if f == "headers" {
			var h map[string]string
			if json.Unmarshal(v, &h) != nil {
				delete(m, f)
				continue
			}
			nonEmpty := false
			for k, val := range h {
				nonEmpty = nonEmpty || val != ""
				h[k] = ""
			}
			if nonEmpty {
				set = append(set, f)
			}
			m[f], _ = json.Marshal(h)
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			set = append(set, f)
		}
		m[f] = json.RawMessage(`""`)
	}
	out, _ := json.Marshal(m)
	return out, set
}

// MergeSecrets supports write-only secrets: the UI sends back the redacted config with secret
// fields left empty unless the user typed a new value. An empty or missing secret field in
// updated keeps the value from old. For webhook headers: a missing or null "headers" keeps all
// old headers; otherwise the updated map decides which headers exist, and an empty value keeps
// that header's old value. (A secret can therefore not be cleared to "" through a merge; delete
// and recreate the channel, or remove the header, instead.)
func MergeSecrets(typ string, old, updated json.RawMessage) (json.RawMessage, error) {
	fields := SecretFields(typ)
	if fields == nil {
		return nil, fmt.Errorf("unknown channel type %q", typ)
	}
	nu, err := object(updated)
	if err != nil {
		return nil, err
	}
	od, err := object(old)
	if err != nil {
		od = map[string]json.RawMessage{} // nothing to keep; don't block saving a fixed config
	}
	for _, f := range fields {
		oldV, hadOld := od[f]
		if f == "headers" {
			newV, ok := nu[f]
			if !ok || string(bytes.TrimSpace(newV)) == "null" {
				if hadOld {
					nu[f] = oldV
				}
				continue
			}
			var nh, oh map[string]string
			if err := json.Unmarshal(newV, &nh); err != nil {
				return nil, errors.New("headers must be an object of strings")
			}
			_ = json.Unmarshal(oldV, &oh)
			for k, v := range nh {
				if v == "" {
					nh[k] = oh[k]
				}
			}
			nu[f], _ = json.Marshal(nh)
			continue
		}
		var s string
		if v, ok := nu[f]; ok && string(bytes.TrimSpace(v)) != "null" {
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, fmt.Errorf("%s must be a string", f)
			}
		}
		if s == "" && hadOld {
			nu[f] = oldV
		}
	}
	return json.Marshal(nu)
}

func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, errors.New("config is not a valid JSON object")
	}
	return m, nil
}
