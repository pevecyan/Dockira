package notify

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := []Channel{
		{Type: TypeNtfy, Name: "n", Config: json.RawMessage(`{"topic":"alerts"}`)},
		{Type: TypeNtfy, Name: "n", Config: json.RawMessage(`{"server":"https://ntfy.example.com","topic":"a-b_c","username":"u","password":"p","tags":["docker"]}`)},
		{Type: TypeGotify, Name: "g", Config: json.RawMessage(`{"server":"http://gotify:80","token":"t"}`)},
		{Type: TypeTelegram, Name: "t", Config: json.RawMessage(`{"bot_token":"123:abc","chat_id":"-100","thread_id":5}`)},
		{Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"https://hooks.example.com/x"}`)},
		{Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"https://x.example","method":"put","preset":"custom","template":"{\"t\":{{json .Title}}}","headers":{"X-Key":"v"}}`)},
	}
	for i, ch := range ok {
		if err := Validate(ch); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string]Channel{
		"no name":         {Type: TypeNtfy, Config: json.RawMessage(`{"topic":"a"}`)},
		"unknown type":    {Type: "email", Name: "x", Config: json.RawMessage(`{}`)},
		"ntfy no topic":   {Type: TypeNtfy, Name: "n", Config: json.RawMessage(`{}`)},
		"ntfy bad topic":  {Type: TypeNtfy, Name: "n", Config: json.RawMessage(`{"topic":"a/b"}`)},
		"ntfy ftp":        {Type: TypeNtfy, Name: "n", Config: json.RawMessage(`{"server":"ftp://x","topic":"a"}`)},
		"ntfy both auths": {Type: TypeNtfy, Name: "n", Config: json.RawMessage(`{"topic":"a","token":"t","username":"u","password":"p"}`)},
		"ntfy user only":  {Type: TypeNtfy, Name: "n", Config: json.RawMessage(`{"topic":"a","username":"u"}`)},
		"gotify no token": {Type: TypeGotify, Name: "g", Config: json.RawMessage(`{"server":"https://g"}`)},
		"gotify no srv":   {Type: TypeGotify, Name: "g", Config: json.RawMessage(`{"token":"t"}`)},
		"tg no chat":      {Type: TypeTelegram, Name: "t", Config: json.RawMessage(`{"bot_token":"1:a"}`)},
		"tg bad token":    {Type: TypeTelegram, Name: "t", Config: json.RawMessage(`{"bot_token":"1/../a","chat_id":"1"}`)},
		"tg bad api":      {Type: TypeTelegram, Name: "t", Config: json.RawMessage(`{"bot_token":"1:a","chat_id":"1","api_base":"file:///etc"}`)},
		"wh no url":       {Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{}`)},
		"wh relative url": {Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"/hook"}`)},
		"wh GET":          {Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"https://x","method":"GET"}`)},
		"wh preset":       {Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"https://x","preset":"teams"}`)},
		"wh no template":  {Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"https://x","preset":"custom"}`)},
		"wh bad template": {Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"https://x","preset":"custom","template":"{{.Title"}`)},
		"wh header name":  {Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"https://x","headers":{"Bad Name":"v"}}`)},
		"wh header crlf":  {Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"https://x","headers":{"X":"a\r\nInjected: 1"}}`)},
		"not json":        {Type: TypeGotify, Name: "g", Config: json.RawMessage(`[1,2]`)},
		"wrong type":      {Type: TypeTelegram, Name: "t", Config: json.RawMessage(`{"bot_token":"1:a","chat_id":"1","thread_id":"x"}`)},
	}
	for name, ch := range bad {
		if err := Validate(ch); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateDoesNotEchoSecrets(t *testing.T) {
	ch := Channel{Type: TypeWebhook, Name: "w", Config: json.RawMessage(`{"url":"https://discord.com/api/webhooks/1/SECRETTOKEN\u007f"}`)}
	err := Validate(ch)
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestSecretFields(t *testing.T) {
	if !reflect.DeepEqual(SecretFields(TypeNtfy), []string{"token", "password"}) ||
		!reflect.DeepEqual(SecretFields(TypeWebhook), []string{"url", "headers"}) ||
		SecretFields("nope") != nil {
		t.Fatal("SecretFields")
	}
}

func TestRedact(t *testing.T) {
	ch := Channel{Type: TypeWebhook, Config: json.RawMessage(`{"url":"https://discord.com/api/webhooks/1/tok","headers":{"Authorization":"Bearer x","X-Empty":""},"preset":"discord"}`)}
	red, set := Redact(ch)
	var got WebhookConfig
	if err := json.Unmarshal(red, &got); err != nil {
		t.Fatal(err)
	}
	if got.URL != "" || got.Preset != "discord" || len(got.Headers) != 2 || got.Headers["Authorization"] != "" {
		t.Errorf("redacted: %s", red)
	}
	if !reflect.DeepEqual(set, []string{"url", "headers"}) {
		t.Errorf("set = %v", set)
	}
	if strings.Contains(string(red), "tok") || strings.Contains(string(red), "Bearer") {
		t.Errorf("secret leaked: %s", red)
	}

	red, set = Redact(Channel{Type: TypeNtfy, Config: json.RawMessage(`{"topic":"a","token":"","password":"pw","username":"u"}`)})
	if !slices.Equal(set, []string{"password"}) || strings.Contains(string(red), "pw") || !strings.Contains(string(red), `"username":"u"`) {
		t.Errorf("ntfy: %s %v", red, set)
	}

	red, set = Redact(Channel{Type: TypeGotify, Config: json.RawMessage(`not json "token"`)})
	if string(red) != "{}" || set != nil {
		t.Errorf("garbage: %s %v", red, set)
	}
}

func TestMergeSecrets(t *testing.T) {
	old := json.RawMessage(`{"topic":"a","token":"old-token"}`)
	// Unchanged secret (sent back blank) keeps the old one; other fields update.
	m, err := MergeSecrets(TypeNtfy, old, json.RawMessage(`{"topic":"b","token":""}`))
	if err != nil {
		t.Fatal(err)
	}
	var c NtfyConfig
	json.Unmarshal(m, &c)
	if c.Topic != "b" || c.Token != "old-token" {
		t.Errorf("merged: %s", m)
	}
	// Missing secret keeps it too; a new value replaces it.
	m, _ = MergeSecrets(TypeNtfy, old, json.RawMessage(`{"topic":"b"}`))
	json.Unmarshal(m, &c)
	if c.Token != "old-token" {
		t.Errorf("missing: %s", m)
	}
	m, _ = MergeSecrets(TypeNtfy, old, json.RawMessage(`{"topic":"b","token":"new"}`))
	json.Unmarshal(m, &c)
	if c.Token != "new" {
		t.Errorf("replace: %s", m)
	}
	// New channel: no old config.
	m, err = MergeSecrets(TypeGotify, nil, json.RawMessage(`{"server":"https://g","token":"t"}`))
	if err != nil || !strings.Contains(string(m), `"token":"t"`) {
		t.Errorf("new: %s %v", m, err)
	}

	// Webhook headers: per-header merge; names in updated decide which exist.
	oldWH := json.RawMessage(`{"url":"https://x/secret","headers":{"A":"1","B":"2"}}`)
	m, err = MergeSecrets(TypeWebhook, oldWH, json.RawMessage(`{"url":"","headers":{"A":"","C":"3"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var w WebhookConfig
	json.Unmarshal(m, &w)
	if w.URL != "https://x/secret" || !reflect.DeepEqual(w.Headers, map[string]string{"A": "1", "C": "3"}) {
		t.Errorf("webhook: %s", m)
	}
	m, _ = MergeSecrets(TypeWebhook, oldWH, json.RawMessage(`{"url":"https://new"}`))
	w = WebhookConfig{}
	json.Unmarshal(m, &w)
	if w.URL != "https://new" || len(w.Headers) != 2 {
		t.Errorf("headers omitted: %s", m)
	}

	// Round trip: Redact then Merge restores the original.
	orig := Channel{Type: TypeWebhook, Config: oldWH}
	red, _ := Redact(orig)
	m, _ = MergeSecrets(TypeWebhook, oldWH, red)
	var a, b any
	json.Unmarshal(m, &a)
	json.Unmarshal(oldWH, &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("round trip: %s", m)
	}

	if _, err := MergeSecrets("nope", nil, json.RawMessage(`{}`)); err == nil {
		t.Error("unknown type accepted")
	}
	if _, err := MergeSecrets(TypeNtfy, old, json.RawMessage(`[]`)); err == nil {
		t.Error("non-object accepted")
	}
	if _, err := MergeSecrets(TypeNtfy, old, json.RawMessage(`{"token":5}`)); err == nil {
		t.Error("non-string secret accepted")
	}
}
