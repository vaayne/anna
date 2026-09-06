package server

import (
	"net/http"

	apiserver "github.com/CherryHQ/stella/api/server"
	pkgchannel "github.com/CherryHQ/stella/pkg/channel"
)

// StartWeixinQR initiates the WeChat QR login flow by requesting a QR code
// from the iLink API. Any authenticated user can call this.
// POST /api/channels/weixin/qr
func (s *Server) StartWeixinQR(w http.ResponseWriter, r *http.Request) {
	qr, err := s.weixinRegistrar.GetQRCode()
	if err != nil {
		s.writeBadGatewayError(w, err)
		return
	}
	writeData(w, http.StatusOK, qr)
}

// PollWeixinQRStatus polls the QR code scan status. On confirmed, it links the
// current user's identity to the Weixin account. Credential provisioning stays
// exclusively in the explicit admin registration flow, which carries a target
// channel ID on every request.
// GET /api/channels/weixin/qr/status?qrcode=...
func (s *Server) PollWeixinQRStatus(w http.ResponseWriter, r *http.Request, params apiserver.PollWeixinQRStatusParams) {
	info := UserFromContext(r.Context())
	if info == nil {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	qrcode := params.Qrcode
	if qrcode == "" {
		writeError(w, http.StatusBadRequest, "qrcode parameter required")
		return
	}

	status, err := s.weixinRegistrar.GetQRCodeStatus(qrcode)
	if err != nil {
		s.writeBadGatewayError(w, err)
		return
	}

	// Identity linking is deliberately independent of channel credentials and
	// therefore never creates or updates channel/plugin rows.
	if status.Status == "confirmed" {
		// Link channel identity if not already linked.
		externalID := status.ILinkUserID
		if externalID == "" {
			externalID = status.ILinkBotID
		}
		if externalID != "" {
			// The Account service owns the idempotent channel-identity link and its
			// best-effort logging.
			s.account.LinkChannelIdentityFromLogin(r.Context(), info.UserID, pkgchannel.PlatformWeixin, externalID, "")
		}
	}

	writeData(w, http.StatusOK, status)
}
