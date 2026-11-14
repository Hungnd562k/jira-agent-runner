// Package telegram is a minimal Telegram Bot API client (long polling) plus message rendering.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls the Telegram Bot API.
type Client struct {
	base string // https://api.telegram.org/bot<token>
	http *http.Client
}

// New creates a client. apiURL is normally "https://api.telegram.org" (overridable for tests).
func New(apiURL, token string) *Client {
	return &Client{
		base: strings.TrimRight(apiURL, "/") + "/bot" + token,
		// Must exceed the long-poll timeout used in GetUpdates.
		http: &http.Client{Timeout: 70 * time.Second},
	}
}

// InlineButton is one inline keyboard button.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// Keyboard is an inline keyboard: rows of buttons. A nil/empty Keyboard removes buttons.
type Keyboard [][]InlineButton

// Update is the subset of a Telegram update the runner uses.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// Message is a Telegram message.
type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

// Chat identifies a chat.
type Chat struct {
	ID int64 `json:"id"`
}

// CallbackQuery is a button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

// APIError is a non-ok Bot API response.
type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// IsNotModified reports whether err is Telegram's harmless "message is not modified".
func IsNotModified(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && strings.Contains(ae.Description, "message is not modified")
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call POSTs a JSON body and decodes result into out. On HTTP 429 it waits retry_after once.
func (c *Client) call(ctx context.Context, method string, body any, out any) error {
	for attempt := 0; ; attempt++ {
		err := c.callOnce(ctx, method, body, out)
		var ae *APIError
		if attempt == 0 && errors.As(err, &ae) && ae.Code == http.StatusTooManyRequests && ae.RetryAfter > 0 {
			select {
			case <-time.After(time.Duration(ae.RetryAfter) * time.Second):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return err
	}
}

func (c *Client) callOnce(ctx context.Context, method string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+method, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// Strip the URL (contains the bot token) from transport errors.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()

	var ar apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d: decode: %w", method, resp.StatusCode, err)
	}
	if !ar.OK {
		ae := &APIError{Method: method, Code: ar.ErrorCode, Description: ar.Description}
		if ar.Parameters != nil {
			ae.RetryAfter = ar.Parameters.RetryAfter
		}
		return ae
	}
	if out != nil {
		return json.Unmarshal(ar.Result, out)
	}
	return nil
}

type replyMarkup struct {
	InlineKeyboard Keyboard `json:"inline_keyboard"`
}

func markup(kb Keyboard) replyMarkup {
	if kb == nil {
		kb = Keyboard{}
	}
	return replyMarkup{InlineKeyboard: kb}
}

// SendMessage sends an HTML message and returns its message id. replyTo=0 means no reply.
func (c *Client) SendMessage(ctx context.Context, chatID int64, html string, kb Keyboard, replyTo int64) (int64, error) {
	body := map[string]any{
		"chat_id":                  chatID,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
		"reply_markup":             markup(kb),
	}
	if replyTo != 0 {
		body["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	var msg Message
	if err := c.call(ctx, "sendMessage", body, &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// EditMessage replaces the text and keyboard of an existing message.
func (c *Client) EditMessage(ctx context.Context, chatID, messageID int64, html string, kb Keyboard) error {
	return c.call(ctx, "editMessageText", map[string]any{
		"chat_id":                  chatID,
		"message_id":               messageID,
		"text":                     html,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
		"reply_markup":             markup(kb),
	}, nil)
}

// AnswerCallback acknowledges a button press (text shows as a toast; may be empty).
func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	}, nil)
}

// GetUpdates long-polls for updates with id >= offset.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(timeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}
