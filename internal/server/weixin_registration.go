package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/CherryHQ/stella/internal/controlplane"
	"github.com/CherryHQ/stella/internal/platform/config"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
)

const weixinRegistrationPollInterval = 2

// errWeixinConfigInvalid wraps plugin config-validation failures so callers can
// distinguish a bad caller-supplied config (HTTP 400) from a store/runtime
// failure (HTTP 500).
var errWeixinConfigInvalid = errors.New("invalid weixin channel config")

type weixinRegistrationBeginRequest struct {
	ChannelID string `json:"channel_id"`
}

type weixinRegistrationPollRequest struct {
	QRCode    string         `json:"qrcode"`
	ChannelID string         `json:"channel_id"`
	AgentID   string         `json:"agent_id"`
	Name      string         `json:"name"`
	Config    map[string]any `json:"config"`
}

func (s *Server) BeginWeixinRegistration(w http.ResponseWriter, r *http.Request) {
	access, ok := s.beginControlPlane(w, r)
	if !ok {
		return
	}
	var req weixinRegistrationBeginRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	channelID := strings.TrimSpace(req.ChannelID)
	if channelID == "" {
		channelID = generateChannelID(pkgchannel.PlatformWeixin)
	}
	operation, err := access.ManageChannel(channelID)
	if err != nil {
		s.writeControlPlaneError(w, err)
		return
	}
	if err := s.validateWeixinTarget(r.Context(), operation); err != nil {
		s.writeControlPlaneError(w, err)
		return
	}
	qr, err := s.weixinRegistrar.GetQRCode()
	if err != nil {
		s.writeBadGatewayError(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{
		"channel_id":    channelID,
		"qrcode":        qr.QRCode,
		"qr_image_url":  qr.QRCodeImgContent,
		"poll_interval": weixinRegistrationPollInterval,
	})
}

func (s *Server) PollWeixinRegistration(w http.ResponseWriter, r *http.Request) {
	access, ok := s.beginControlPlane(w, r)
	if !ok {
		return
	}
	var req weixinRegistrationPollRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.QRCode = strings.TrimSpace(req.QRCode)
	req.ChannelID = strings.TrimSpace(req.ChannelID)
	req.AgentID = strings.TrimSpace(req.AgentID)
	if req.QRCode == "" {
		writeError(w, http.StatusBadRequest, "qrcode is required")
		return
	}
	if req.ChannelID == "" {
		writeError(w, http.StatusBadRequest, "channel_id is required")
		return
	}
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required; bind this WeChat channel to an agent")
		return
	}
	operation, err := access.ManageChannel(req.ChannelID)
	if err != nil {
		s.writeControlPlaneError(w, err)
		return
	}
	if err := s.validateWeixinTarget(r.Context(), operation); err != nil {
		s.writeControlPlaneError(w, err)
		return
	}
	prospective := config.Channel{ID: req.ChannelID, Type: pkgchannel.PlatformWeixin, AgentID: req.AgentID}
	agent, err := access.LookupAgent(r.Context(), req.AgentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "agent_id must reference an existing agent")
		return
	}
	if !agent.Enabled {
		writeError(w, http.StatusBadRequest, "agent_id must reference an enabled agent")
		return
	}
	if err := operation.ValidateBinding(r.Context(), prospective); err != nil {
		s.writeControlPlaneError(w, err)
		return
	}

	status, err := s.weixinRegistrar.GetQRCodeStatus(req.QRCode)
	if err != nil {
		s.writeBadGatewayError(w, err)
		return
	}
	if status.Status != "confirmed" {
		writeData(w, http.StatusOK, map[string]any{"status": status.Status, "poll_interval": weixinRegistrationPollInterval})
		return
	}
	if status.BotToken == "" {
		writeError(w, http.StatusBadGateway, "WeChat registration did not return bot credentials")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "WeChat"
	}
	saved, err := s.saveWeixinChannel(r.Context(), operation, name, req.AgentID, true, req.Config, status)
	if errors.Is(err, errWeixinConfigInvalid) {
		writeError(w, http.StatusBadRequest, "invalid channel config")
		return
	}
	if err != nil {
		s.writeControlPlaneError(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{"status": "created", "channel": channelToView(saved)})
}

// validateWeixinTarget permits an existing ordinary channel row named
// "weixin" for compatibility, while rejecting a target that belongs to a
// different platform before any credentials can be merged.
func (s *Server) validateWeixinTarget(ctx context.Context, operation *controlplane.ChannelManagement) error {
	ch, err := operation.Channel(ctx, operation.ID())
	if err == nil {
		channelType := ch.Type
		if channelType == "" {
			channelType = ch.ID
		}
		if channelType != pkgchannel.PlatformWeixin {
			return &controlplane.ValidationError{Msg: fmt.Sprintf("channel %q is not a weixin channel", ch.ID)}
		}
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("load weixin channel: %w", err)
	}
	return nil
}

// saveWeixinChannel writes credentials to the exact channel selected by the
// registration request. Existing rows are merged after their type was checked;
// missing rows are created with the requested ID and Weixin type.
func (s *Server) saveWeixinChannel(ctx context.Context, operation *controlplane.ChannelManagement, name, agentID string, enable bool, cfgPatch map[string]any, status WeixinQRCodeStatus) (config.Channel, error) {
	if operation == nil {
		return config.Channel{}, errors.New("missing authorized channel operation")
	}
	channelID := operation.ID()
	ch, err := operation.Channel(ctx, channelID)
	create := isNotFound(err)
	if err != nil && !create {
		return config.Channel{}, fmt.Errorf("load weixin channel: %w", err)
	}
	if !create {
		channelType := ch.Type
		if channelType == "" {
			channelType = ch.ID
		}
		if channelType != pkgchannel.PlatformWeixin {
			return config.Channel{}, &controlplane.ValidationError{Msg: fmt.Sprintf("channel %q is not a weixin channel", ch.ID)}
		}
	}
	cfg := map[string]any{}
	if create {
		ch = config.Channel{ID: channelID, Type: pkgchannel.PlatformWeixin, Enabled: true}
	} else if ch.Config != "" {
		_ = json.Unmarshal([]byte(ch.Config), &cfg)
		if cfg == nil {
			cfg = map[string]any{}
		}
	}
	if ch.Type == "" {
		ch.Type = pkgchannel.PlatformWeixin
	}
	if name != "" {
		ch.Name = name
	}
	if agentID != "" {
		ch.AgentID = agentID
	}
	if enable {
		ch.Enabled = true
	}
	maps.Copy(cfg, cfgPatch)
	cfg["bot_token"] = status.BotToken
	cfg["base_url"] = status.BaseURL
	cfg["bot_id"] = status.ILinkBotID
	cfg["user_id"] = status.ILinkUserID

	ch.ID = channelID
	ch.Type = pkgchannel.PlatformWeixin
	if s.pluginHost != nil {
		if err := s.pluginHost.ValidateConfig(config.PluginID(config.PluginKindChannel, ch.Type), cfg); err != nil {
			return config.Channel{}, fmt.Errorf("%w: %w", errWeixinConfigInvalid, err)
		}
	}
	return operation.Save(ctx, ch, cfg, create)
}
