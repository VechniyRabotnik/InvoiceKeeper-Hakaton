package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/app"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
)

// httpDoer — минимальный транспорт. Подменяется в тестах, чтобы проверять
// разбор ответов Bot API без сети.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Telegram — канал доставки через Bot API: напоминания, ответы на команды и
// приём нажатий inline-кнопки «Оплачено».
type Telegram struct {
	token   string
	baseURL string
	http    httpDoer
}

// NewTelegram создаёт канал. Без токена возвращается nil-интерфейс:
// типизированный nil-указатель в интерфейсе Notifier не равен nil, и
// планировщик считал бы бота включённым.
//
// chat_id здесь не нужен: подписчики берутся из таблицы telegram_chats,
// которую наполняет команда /start.
func NewTelegram(token string) Notifier {
	if token == "" {
		return nil
	}
	return &Telegram{
		token:   token,
		baseURL: "https://api.telegram.org",
		http:    &http.Client{Timeout: 40 * time.Second},
	}
}

// Notify публикует напоминание с кнопкой «Оплачено». callback_data содержит
// идентификатор периода, поэтому нажатие однозначно определяет, что оплачено.
func (t *Telegram) Notify(chatID string, o domain.Occurrence, today domain.Day) error {
	return t.call("sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    app.ReminderText(o, today),
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]string{{
				{"text": "✅ Оплачено", "callback_data": "paid:" + strconv.FormatInt(o.ID, 10)},
			}},
		},
	}, nil)
}

// SendText отвечает текстом — используется командами бота. Меню команд
// задаётся один раз через RegisterCommands, поэтому дублировать клавиатуру в
// каждом сообщении не нужно.
func (t *Telegram) SendText(chatID, text string) error {
	return t.call("sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil)
}

// Updates забирает обновления начиная с offset: и нажатия кнопок, и сообщения
// с командами.
//
// Используется короткий long polling: запрос ждёт события до 25 секунд, что
// даёт мгновенную реакцию и не создаёт лишнюю нагрузку.
func (t *Telegram) Updates(offset int64) ([]Update, error) {
	query := url.Values{
		"offset":  {strconv.FormatInt(offset, 10)},
		"timeout": {"25"},
	}

	var payload struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID      int64 `json:"update_id"`
			CallbackQuery *struct {
				ID      string `json:"id"`
				Data    string `json:"data"`
				Message *struct {
					MessageID int64 `json:"message_id"`
					Chat      struct {
						ID int64 `json:"id"`
					} `json:"chat"`
				} `json:"message"`
			} `json:"callback_query"`
			Message *struct {
				Text string `json:"text"`
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := t.call("getUpdates", nil, &payload, query); err != nil {
		return nil, err
	}

	out := make([]Update, 0, len(payload.Result))
	for _, u := range payload.Result {
		if u.CallbackQuery != nil && u.CallbackQuery.Message != nil {
			out = append(out, Update{
				UpdateID: u.UpdateID,
				Callback: &Callback{
					UpdateID:  u.UpdateID,
					ID:        u.CallbackQuery.ID,
					Data:      u.CallbackQuery.Data,
					ChatID:    strconv.FormatInt(u.CallbackQuery.Message.Chat.ID, 10),
					MessageID: u.CallbackQuery.Message.MessageID,
				},
			})
			continue
		}
		// Команды приходят текстом сообщения; пустой текст — стикер или фото.
		if u.Message != nil && strings.HasPrefix(u.Message.Text, "/") {
			out = append(out, Update{
				UpdateID: u.UpdateID,
				Command: &Command{
					UpdateID: u.UpdateID,
					ChatID:   strconv.FormatInt(u.Message.Chat.ID, 10),
					Text:     u.Message.Text,
				},
			})
		}
	}
	return out, nil
}

// Ack подтверждает нажатие: без этого Telegram показывает «часики» у клиента.
func (t *Telegram) Ack(callbackID, text string) error {
	return t.call("answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	}, nil)
}

// ClearButtons убирает клавиатуру у обработанного сообщения, чтобы оплаченный
// период нельзя было отметить повторно случайным нажатием.
func (t *Telegram) ClearButtons(chatID string, messageID int64) error {
	return t.call("editMessageReplyMarkup", map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": map[string]any{"inline_keyboard": [][]any{}},
	}, nil)
}

// RegisterCommands регистрирует список команд в меню бота, чтобы он был
// понятен без чтения документации. Вызывается при старте.
func (t *Telegram) RegisterCommands() error {
	return t.call("setMyCommands", map[string]any{
		"commands": []map[string]string{
			{"command": "start", "description": "Получать напоминания об оплате"},
			{"command": "summary", "description": "Итоги и ближайшие платежи"},
			{"command": "payments", "description": "Мои платежи (можно указать месяц, напр: /payments 2026-11)"},
			{"command": "pay", "description": "Выбрать платеж и отметить его оплаченным"},
			{"command": "unpay", "description": "Отменить отметку об оплате платежа"},
			{"command": "setdate", "description": "Указать/изменить дату оплаты (формат: /setdate ID 2026-10-24)"},
			{"command": "overdue", "description": "Мои просрочки по оплате"},
			{"command": "calendar", "description": "Календарь оплат на месяц (можно указать месяц, напр: /calendar 2026-11)"},
			{"command": "budget", "description": "Плановый бюджет на месяц или год"},
			{"command": "id", "description": "ID этого чата"},
			{"command": "unsubscribe", "description": "Отключить напоминания"},
			{"command": "help", "description": "Список команд"},
		},
	}, nil)
}

// call выполняет метод Bot API. payload == nil означает GET с query.
func (t *Telegram) call(method string, payload any, out any, query ...url.Values) error {
	endpoint := fmt.Sprintf("%s/bot%s/%s", t.baseURL, t.token, method)
	if len(query) > 0 {
		endpoint += "?" + query[0].Encode()
	}

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(raw, &apiErr)
		return fmt.Errorf("telegram %s: %d %s", method, resp.StatusCode, apiErr.Description)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
