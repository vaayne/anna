package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vaayne/pibot/agent"
)

const (
	telegramMaxMessageLen = 4000
	longPollTimeout       = 30
	retryDelay            = 5 * time.Second
)

type telegramUpdate struct {
	UpdateID int              `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramMessage struct {
	Chat telegramChat `json:"chat"`
	Text string       `json:"text"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

type telegramGetUpdatesResponse struct {
	OK     bool             `json:"ok"`
	Result []telegramUpdate `json:"result"`
}

type telegramSendMessageRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

type telegramSendChatActionRequest struct {
	ChatID int64  `json:"chat_id"`
	Action string `json:"action"`
}

// RunTelegram starts a Telegram bot using long polling. It blocks until ctx is
// cancelled. Messages are processed sequentially.
func RunTelegram(ctx context.Context, token string, sm *agent.SessionManager) error {
	return runTelegramLoop(ctx, "https://api.telegram.org/bot"+token, &http.Client{}, sm)
}

func runTelegramLoop(ctx context.Context, baseURL string, client *http.Client, sm *agent.SessionManager) error {
	offset := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		updates, err := getUpdates(ctx, client, baseURL, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("telegram: getUpdates error: %v, retrying in %s", err, retryDelay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
			}
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}

			if u.Message == nil || strings.TrimSpace(u.Message.Text) == "" {
				continue
			}

			chatID := u.Message.Chat.ID
			sessionID := strconv.FormatInt(chatID, 10)
			text := u.Message.Text

			// Send typing indicator (best-effort).
			_ = sendChatAction(ctx, client, baseURL, chatID, "typing")

			ag, err := sm.GetOrCreate(ctx, sessionID)
			if err != nil {
				log.Printf("telegram: new session %s error: %v", sessionID, err)
				_ = sendMessage(ctx, client, baseURL, chatID, fmt.Sprintf("Error starting agent: %v", err))
				continue
			}
			log.Printf("telegram: session %s active, processing message", sessionID)

			// Collect streaming response.
			var sb strings.Builder
			var streamErr error
			for evt := range ag.SendPrompt(ctx, text) {
				if evt.Err != nil {
					streamErr = evt.Err
					break
				}
				sb.WriteString(evt.Text)
			}

			response := sb.String()
			if streamErr != nil {
				log.Printf("telegram: agent error for session %s: %v", sessionID, streamErr)
				if response == "" {
					response = fmt.Sprintf("Agent error: %v", streamErr)
				} else {
					response += fmt.Sprintf("\n\n[Agent error: %v]", streamErr)
				}
			}

			if strings.TrimSpace(response) == "" {
				response = "(empty response)"
			}

			// Split and send.
			chunks := splitMessage(response)
			for _, chunk := range chunks {
				if err := sendMessage(ctx, client, baseURL, chatID, chunk); err != nil {
					log.Printf("telegram: sendMessage error for chat %d: %v", chatID, err)
				}
			}
		}
	}
}

// getUpdates calls the Telegram getUpdates API with long polling.
func getUpdates(ctx context.Context, client *http.Client, baseURL string, offset int) ([]telegramUpdate, error) {
	url := fmt.Sprintf("%s/getUpdates?offset=%d&timeout=%d", baseURL, offset, longPollTimeout)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result telegramGetUpdatesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram API returned ok=false: %s", string(body))
	}

	return result.Result, nil
}

// sendMessage sends a text message to a Telegram chat, returning any error.
func sendMessage(ctx context.Context, client *http.Client, baseURL string, chatID int64, text string) error {
	payload := telegramSendMessageRequest{
		ChatID: chatID,
		Text:   text,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	url := baseURL + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// sendChatAction sends a chat action (e.g. "typing") to a Telegram chat.
func sendChatAction(ctx context.Context, client *http.Client, baseURL string, chatID int64, action string) error {
	payload := telegramSendChatActionRequest{
		ChatID: chatID,
		Action: action,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	url := baseURL + "/sendChatAction"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	return nil
}

// splitMessage splits a message into chunks that fit within Telegram's 4096
// character limit. It tries to split at newline boundaries when possible.
func splitMessage(text string) []string {
	if len(text) <= telegramMaxMessageLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= telegramMaxMessageLen {
			chunks = append(chunks, text)
			break
		}

		cutAt := telegramMaxMessageLen
		// Try to find the last newline before the limit.
		if idx := strings.LastIndex(text[:cutAt], "\n"); idx > 0 {
			cutAt = idx + 1 // Include the newline in the current chunk.
		}

		chunks = append(chunks, text[:cutAt])
		text = text[cutAt:]
	}

	return chunks
}
