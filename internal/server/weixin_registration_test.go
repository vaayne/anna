package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/server"
)

func seedWeixinRegistrationChannel(t *testing.T, env *testEnv, id, agentID string) {
	t.Helper()
	if err := env.store.CreateChannel(context.Background(), config.Channel{
		ID: id, Type: "weixin", AgentID: agentID, Config: `{}`,
	}); err != nil {
		t.Fatalf("CreateChannel(%s): %v", id, err)
	}
}

func TestBeginWeixinRegistrationProxiesQRCode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/get_bot_qrcode" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("bot_type"); got != "3" {
			t.Fatalf("bot_type = %q, want 3", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ret":                0,
			"qrcode":             "qr-1",
			"qrcode_img_content": "https://wx.example/qr-1",
		})
	}))
	defer upstream.Close()
	defer server.SetWeixinRegistrationEndpointForTesting(upstream.URL)()

	env := setupAdmin(t)
	seedWeixinRegistrationChannel(t, env, "weixin-register", "")
	rr := doRequest(t, env, "POST", "/api/channels/weixin/register/qr", map[string]any{"channel_id": "weixin-register"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
	}
	var got struct {
		ChannelID    string `json:"channel_id"`
		QRCode       string `json:"qrcode"`
		QRImageURL   string `json:"qr_image_url"`
		PollInterval int    `json:"poll_interval"`
	}
	if err := json.Unmarshal(parseResponse(t, rr).Data, &got); err != nil {
		t.Fatalf("unmarshal begin: %v", err)
	}
	if got.ChannelID != "weixin-register" || got.QRCode != "qr-1" || got.QRImageURL != "https://wx.example/qr-1" || got.PollInterval != 2 {
		t.Fatalf("begin response = %#v", got)
	}
}

func TestPollWeixinRegistrationCreatesChannel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/get_qrcode_status" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("qrcode"); got != "qr-1" {
			t.Fatalf("qrcode = %q, want qr-1", got)
		}
		if got := r.Header.Get("iLink-App-ClientVersion"); got == "" {
			t.Fatalf("missing iLink-App-ClientVersion header")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ret":           0,
			"status":        "confirmed",
			"bot_token":     "wx-token",
			"ilink_bot_id":  "bot-1",
			"ilink_user_id": "user-1",
			"baseurl":       "https://wx.example",
		})
	}))
	defer upstream.Close()
	defer server.SetWeixinRegistrationEndpointForTesting(upstream.URL)()

	env := setupAdmin(t)
	agentID := findStellaID(t, env)
	seedWeixinRegistrationChannel(t, env, "weixin-a", "")
	rr := doRequest(t, env, "POST", "/api/channels/weixin/register/poll", map[string]any{
		"qrcode":     "qr-1",
		"channel_id": "weixin-a",
		"agent_id":   agentID,
		"name":       "WeChat Auto",
		"config": map[string]any{
			"sk_route_tag": "stella-test",
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
	}
	var got struct {
		Status  string `json:"status"`
		Channel struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			AgentID string `json:"agent_id"`
			Config  string `json:"config"`
		} `json:"channel"`
	}
	if err := json.Unmarshal(parseResponse(t, rr).Data, &got); err != nil {
		t.Fatalf("unmarshal poll: %v", err)
	}
	if got.Status != "created" || got.Channel.ID != "weixin-a" || got.Channel.Type != "weixin" || got.Channel.AgentID != agentID {
		t.Fatalf("poll response = %#v", got)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(got.Channel.Config), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg["bot_token"] != "wx-token" || cfg["base_url"] != "https://wx.example" || cfg["bot_id"] != "bot-1" || cfg["user_id"] != "user-1" || cfg["sk_route_tag"] != "stella-test" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestPollWeixinRegistrationKeepsCredentialsPerInstance(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := "token-a"
		botID := "bot-a"
		if r.URL.Query().Get("qrcode") == "qr-b" {
			token = "token-b"
			botID = "bot-b"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ret": 0, "status": "confirmed", "bot_token": token,
			"ilink_bot_id": botID, "ilink_user_id": "user-" + botID,
			"baseurl": "https://wx.example",
		})
	}))
	defer upstream.Close()
	defer server.SetWeixinRegistrationEndpointForTesting(upstream.URL)()

	env := setupAdmin(t)
	agentA := findStellaID(t, env)
	agentB := "weixin-agent-b"
	if err := env.store.CreateAgent(context.Background(), config.Agent{ID: agentB, Name: "Weixin B", Model: "test", Enabled: true}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	for _, tc := range []struct {
		qr, id, agent, token string
	}{
		{qr: "qr-a", id: "weixin-a", agent: agentA, token: "token-a"},
		{qr: "qr-b", id: "weixin-b", agent: agentB, token: "token-b"},
	} {
		seedWeixinRegistrationChannel(t, env, tc.id, "")
		rr := doRequest(t, env, "POST", "/api/channels/weixin/register/poll", map[string]any{
			"qrcode": tc.qr, "channel_id": tc.id, "agent_id": tc.agent,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d (body: %s)", tc.id, rr.Code, rr.Body.String())
		}
		ch, err := env.store.GetChannel(context.Background(), tc.id)
		if err != nil {
			t.Fatalf("GetChannel(%s): %v", tc.id, err)
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(ch.Config), &cfg); err != nil {
			t.Fatalf("config %s: %v", tc.id, err)
		}
		if cfg["bot_token"] != tc.token {
			t.Fatalf("%s bot_token mismatch", tc.id)
		}
	}
	channelA, err := env.store.GetChannel(context.Background(), "weixin-a")
	if err != nil {
		t.Fatal(err)
	}
	var cfgA map[string]any
	if err := json.Unmarshal([]byte(channelA.Config), &cfgA); err != nil {
		t.Fatal(err)
	}
	if cfgA["bot_token"] != "token-a" {
		t.Fatal("saving instance B changed instance A credentials")
	}
}

func TestPollWeixinQRStatusDoesNotProvisionChannelCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ret": 0, "status": "confirmed", "bot_token": "wx-secret",
			"ilink_bot_id": "bot-identity", "ilink_user_id": "user-identity",
		})
	}))
	defer upstream.Close()
	defer server.SetWeixinRegistrationEndpointForTesting(upstream.URL)()

	env := setupAdmin(t)
	beforeChannels, err := env.store.ListChannels(context.Background())
	if err != nil {
		t.Fatalf("ListChannels before: %v", err)
	}
	beforeOverrides, err := env.store.ListPluginOverrides(context.Background())
	if err != nil {
		t.Fatalf("ListPluginOverrides before: %v", err)
	}
	rr := doRequest(t, env, "GET", "/api/channels/weixin/qr/status?qrcode=qr-identity", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	afterChannels, err := env.store.ListChannels(context.Background())
	if err != nil {
		t.Fatalf("ListChannels after: %v", err)
	}
	if !reflect.DeepEqual(beforeChannels, afterChannels) {
		t.Fatalf("identity link changed channel rows")
	}
	afterOverrides, err := env.store.ListPluginOverrides(context.Background())
	if err != nil {
		t.Fatalf("ListPluginOverrides after: %v", err)
	}
	if !reflect.DeepEqual(beforeOverrides, afterOverrides) {
		t.Fatalf("identity link changed plugin override rows")
	}
	for _, plugin := range afterOverrides {
		if got := plugin.Config["bot_token"]; got == "wx-secret" {
			t.Fatal("identity link persisted QR credentials")
		}
	}
}

func TestPollWeixinRegistrationDeniesNonAdminBeforeAgentLookup(t *testing.T) {
	env := setupAdmin(t)
	ctx := context.Background()
	for _, agent := range []config.Agent{
		{ID: "weixin-enabled", Name: "Enabled", Model: "test", Enabled: true},
		{ID: "weixin-disabled", Name: "Disabled", Model: "test", Enabled: false},
	} {
		if err := env.store.CreateAgent(ctx, agent); err != nil {
			t.Fatalf("CreateAgent(%s): %v", agent.ID, err)
		}
	}
	_, token := createTestUserWithToken(t, env.authStore, env.oidcStore, "weixin-regular", auth.RoleUser)
	for _, agentID := range []string{"missing-agent", "weixin-disabled", "weixin-enabled"} {
		rr := doRequestWithSession(t, env.srv, token, "POST", "/api/channels/weixin/register/poll", map[string]any{
			"qrcode": "qr", "channel_id": "weixin-a", "agent_id": agentID,
		})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("agent %q: status = %d, want 403 (body: %s)", agentID, rr.Code, rr.Body.String())
		}
	}
}

func TestPollWeixinRegistrationRequiresChannelID(t *testing.T) {
	env := setupAdmin(t)
	rr := doRequest(t, env, "POST", "/api/channels/weixin/register/poll", map[string]any{
		"qrcode":   "qr-1",
		"agent_id": findStellaID(t, env),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	if got := parseResponse(t, rr).Error; got != "channel_id is required" {
		t.Fatalf("error = %q", got)
	}
}

func TestPollWeixinRegistrationRequiresAgent(t *testing.T) {
	env := setupAdmin(t)
	rr := doRequest(t, env, "POST", "/api/channels/weixin/register/poll", map[string]any{
		"qrcode": "qr-1", "channel_id": "weixin-a",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	if got := parseResponse(t, rr).Error; got != "agent_id is required; bind this WeChat channel to an agent" {
		t.Fatalf("error = %q", got)
	}
}

func TestPollWeixinRegistrationRejectsDifferentPlatformTarget(t *testing.T) {
	env := setupAdmin(t)
	if err := env.store.CreateChannel(context.Background(), config.Channel{
		ID: "telegram-target", Type: "telegram", Name: "Telegram target",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	rr := doRequest(t, env, "POST", "/api/channels/weixin/register/poll", map[string]any{
		"qrcode": "qr-1", "channel_id": "telegram-target", "agent_id": findStellaID(t, env),
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	if got := parseResponse(t, rr).Error; got != `channel "telegram-target" is not a weixin channel` {
		t.Fatalf("error = %q", got)
	}
}

func TestPollWeixinRegistrationRejectsSamePlatformAgentBinding(t *testing.T) {
	env := setupAdmin(t)
	agentID := findStellaID(t, env)
	seedWeixinRegistrationChannel(t, env, "weixin-new", "")
	if err := env.store.CreateChannel(context.Background(), config.Channel{
		ID:      "weixin-existing",
		Name:    "Existing WeChat",
		Type:    "weixin",
		AgentID: agentID,
		Enabled: true,
		Config:  `{"bot_token":"existing"}`,
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	rr := doRequest(t, env, "POST", "/api/channels/weixin/register/poll", map[string]any{
		"qrcode": "qr-1", "channel_id": "weixin-new", "agent_id": agentID,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	if got := parseResponse(t, rr).Error; got != "agent is already bound to weixin channel weixin-existing" {
		t.Fatalf("error = %q", got)
	}
}

func TestPollWeixinRegistrationPendingDoesNotCreateChannel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0, "status": "wait"})
	}))
	defer upstream.Close()
	defer server.SetWeixinRegistrationEndpointForTesting(upstream.URL)()

	env := setupAdmin(t)
	seedWeixinRegistrationChannel(t, env, "weixin-pending", "")
	rr := doRequest(t, env, "POST", "/api/channels/weixin/register/poll", map[string]any{
		"qrcode": "qr-1", "channel_id": "weixin-pending", "agent_id": findStellaID(t, env),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
	}
	var got struct {
		Status       string `json:"status"`
		PollInterval int    `json:"poll_interval"`
	}
	if err := json.Unmarshal(parseResponse(t, rr).Data, &got); err != nil {
		t.Fatalf("unmarshal pending: %v", err)
	}
	if got.Status != "wait" || got.PollInterval != 2 {
		t.Fatalf("pending response = %#v", got)
	}
	ch, err := env.store.GetChannel(context.Background(), "weixin-pending")
	if err != nil {
		t.Fatalf("GetChannel pending: %v", err)
	}
	if ch.Config != `{}` {
		t.Fatalf("channel config changed while registration was pending: %s", ch.Config)
	}
}
