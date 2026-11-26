package telegram

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Chat is the subset of the Chat object we use.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Message is the subset of the Message object we use.
type Message struct {
	MessageID int64         `json:"message_id"`
	From      *User         `json:"from,omitempty"`
	Chat      Chat          `json:"chat"`
	Date      int64         `json:"date"`
	Text      string        `json:"text,omitempty"`
	ReplyTo   *Message      `json:"reply_to_message,omitempty"`
	Entities  []Entity      `json:"entities,omitempty"`
	Markup    *InlineMarkup `json:"reply_markup,omitempty"`
}

// CallbackQuery is an inline button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data"`
}

// Update is one item from getUpdates.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	EditedMessage *Message       `json:"edited_message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Entity is a MessageEntity. Offsets and lengths are in UTF-16 code units.
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// Button is an InlineKeyboardButton with callback data only (no URL
// buttons: nothing from a mail ever becomes a link).
type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// InlineMarkup is an InlineKeyboardMarkup.
type InlineMarkup struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}

type linkPreviewOptions struct {
	IsDisabled bool `json:"is_disabled"`
}

type replyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
}

// Text is a message body with its entities. No parse_mode is ever used, so
// mail content cannot be interpreted as markup.
type Text struct {
	Body     string
	Entities []Entity
}

type sendMessageParams struct {
	ChatID      int64              `json:"chat_id"`
	Text        string             `json:"text"`
	Entities    []Entity           `json:"entities,omitempty"`
	LinkPreview linkPreviewOptions `json:"link_preview_options"`
	ReplyMarkup *InlineMarkup      `json:"reply_markup,omitempty"`
	ReplyParams *replyParameters   `json:"reply_parameters,omitempty"`
	Silent      bool               `json:"disable_notification,omitempty"`
}

// SendOptions tunes SendMessage.
type SendOptions struct {
	Markup  *InlineMarkup
	ReplyTo int64
	Silent  bool
}

// SendMessage sends text to chatID with link previews disabled.
func (c *Client) SendMessage(ctx context.Context, chatID int64, t Text, o SendOptions) (*Message, error) {
	p := sendMessageParams{
		ChatID: chatID, Text: t.Body, Entities: t.Entities,
		LinkPreview: linkPreviewOptions{IsDisabled: true},
		ReplyMarkup: o.Markup, Silent: o.Silent,
	}
	if o.ReplyTo != 0 {
		p.ReplyParams = &replyParameters{MessageID: o.ReplyTo, AllowSendingWithoutReply: true}
	}
	var m Message
	if err := c.callRetry(ctx, "sendMessage", p, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

type editMessageTextParams struct {
	ChatID      int64              `json:"chat_id"`
	MessageID   int64              `json:"message_id"`
	Text        string             `json:"text"`
	Entities    []Entity           `json:"entities,omitempty"`
	LinkPreview linkPreviewOptions `json:"link_preview_options"`
	ReplyMarkup *InlineMarkup      `json:"reply_markup,omitempty"`
}

// EditMessageText replaces a message's text and keyboard.
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, t Text, markup *InlineMarkup) error {
	p := editMessageTextParams{ChatID: chatID, MessageID: messageID, Text: t.Body, Entities: t.Entities,
		LinkPreview: linkPreviewOptions{IsDisabled: true}, ReplyMarkup: markup}
	err := c.callRetry(ctx, "editMessageText", p, nil)
	if isNotModified(err) {
		return nil
	}
	return err
}

// EditMarkup replaces only the inline keyboard (nil removes it).
func (c *Client) EditMarkup(ctx context.Context, chatID, messageID int64, markup *InlineMarkup) error {
	if markup == nil {
		markup = &InlineMarkup{InlineKeyboard: [][]Button{}}
	}
	err := c.callRetry(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id": chatID, "message_id": messageID, "reply_markup": markup,
	}, nil)
	if isNotModified(err) {
		return nil
	}
	return err
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func isNotModified(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == 400 && containsFold(ae.Description, "message is not modified")
}

// AnswerCallback acknowledges a button press, optionally with a toast.
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

// GetUpdates long-polls for updates.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": int(timeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

// BotCommand is one entry of the command menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetMyCommands publishes the command menu.
func (c *Client) SetMyCommands(ctx context.Context, cmds []BotCommand) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// callRetry retries once on 429 (flood control) after the advised delay,
// capped at 30 s.
func (c *Client) callRetry(ctx context.Context, method string, params, out any) error {
	err := c.call(ctx, method, params, out)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == 429 {
		d := time.Duration(ae.RetryAfter) * time.Second
		if d <= 0 || d > 30*time.Second {
			d = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
		return c.call(ctx, method, params, out)
	}
	return err
}
