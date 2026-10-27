// Package tgapi is a minimal Telegram Bot API client.
package tgapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Client calls the Bot API with one token.
type Client struct {
	Token   string
	BaseURL string // default https://api.telegram.org
	HTTP    *http.Client
}

// Ready reports whether a token is set.
func (c *Client) Ready() bool { return c != nil && c.Token != "" }

// Error is an error answer from Telegram.
type Error struct {
	Code        int
	Description string
}

func (e *Error) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Description) }

func (c *Client) url(method string) string {
	base := c.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	return strings.TrimRight(base, "/") + "/bot" + c.Token + "/" + method
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return fmt.Errorf("telegram: bad response (%d)", resp.StatusCode)
	}
	if !r.OK {
		return &Error{Code: r.ErrorCode, Description: r.Description}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// Call invokes a method with JSON parameters.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	if !c.Ready() {
		return errors.New("未配置 Telegram Bot Token")
	}
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		// The URL holds the token: never let it reach logs.
		return errors.New("telegram: 连接失败")
	}
	return decode(resp, out)
}

// SendDocument uploads a file.
func (c *Client) SendDocument(ctx context.Context, chatID int64, name string, data []byte, caption string) error {
	if !c.Ready() {
		return errors.New("未配置 Telegram Bot Token")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("chat_id", fmt.Sprint(chatID))
	if caption != "" {
		w.WriteField("caption", caption)
	}
	fw, _ := w.CreateFormFile("document", name)
	fw.Write(data)
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("sendDocument"), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.http().Do(req)
	if err != nil {
		return errors.New("telegram: 连接失败")
	}
	return decode(resp, nil)
}

// InlineButton is an inline keyboard button.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
	CopyText     *struct {
		Text string `json:"text"`
	} `json:"copy_text,omitempty"`
	Pay bool `json:"pay,omitempty"`
}

// CopyButton returns a button that copies text to the clipboard.
func CopyButton(label, text string) InlineButton {
	b := InlineButton{Text: label}
	b.CopyText = &struct {
		Text string `json:"text"`
	}{Text: text}
	return b
}

// Keyboard is an inline keyboard markup.
type Keyboard struct {
	InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
}

// Message is the subset of a message Kaku reads.
type Message struct {
	MessageID int64 `json:"message_id"`
	From      *User `json:"from"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text              string             `json:"text"`
	ReplyToMessage    *Message           `json:"reply_to_message"`
	SuccessfulPayment *SuccessfulPayment `json:"successful_payment"`
}

// User is a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// SuccessfulPayment is a completed Stars payment.
type SuccessfulPayment struct {
	Currency                string `json:"currency"`
	TotalAmount             int64  `json:"total_amount"`
	InvoicePayload          string `json:"invoice_payload"`
	TelegramPaymentChargeID string `json:"telegram_payment_charge_id"`
}

// CallbackQuery is an inline button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// PreCheckoutQuery is sent before a Stars payment is charged.
type PreCheckoutQuery struct {
	ID             string `json:"id"`
	From           User   `json:"from"`
	Currency       string `json:"currency"`
	TotalAmount    int64  `json:"total_amount"`
	InvoicePayload string `json:"invoice_payload"`
}

// Update is an incoming update.
type Update struct {
	UpdateID         int64             `json:"update_id"`
	Message          *Message          `json:"message"`
	CallbackQuery    *CallbackQuery    `json:"callback_query"`
	PreCheckoutQuery *PreCheckoutQuery `json:"pre_checkout_query"`
}

// SendMessage sends text (HTML parse mode when html is true).
func (c *Client) SendMessage(ctx context.Context, chatID any, text string, html bool, kb *Keyboard) error {
	p := map[string]any{"chat_id": chatID, "text": text, "disable_web_page_preview": true}
	if html {
		p["parse_mode"] = "HTML"
	}
	if kb != nil {
		p["reply_markup"] = kb
	}
	return c.Call(ctx, "sendMessage", p, nil)
}

// EditMessage replaces a message's text and keyboard.
func (c *Client) EditMessage(ctx context.Context, chatID, msgID int64, text string, kb *Keyboard) error {
	p := map[string]any{"chat_id": chatID, "message_id": msgID, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if kb != nil {
		p["reply_markup"] = kb
	}
	return c.Call(ctx, "editMessageText", p, nil)
}
