package bot

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aeza/invoice-keeper/internal/domain"
)

// fakeAPI поднимает сервер, повторяющий форму ответов Telegram Bot API, и
// записывает вызовы — так транспорт проверяется без сети.
type fakeAPI struct {
	*httptest.Server
	calls   []string
	queries []string
	bodies  []map[string]any
	status  int
	reply   string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()

	f := &fakeAPI{status: http.StatusOK, reply: `{"ok":true}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.calls = append(f.calls, method)
		f.queries = append(f.queries, r.URL.RawQuery)

		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		f.bodies = append(f.bodies, body)

		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.reply))
	}))
	t.Cleanup(f.Close)
	return f
}

// newTelegramForTests собирает канал, направленный на тестовый сервер.
func newTelegramForTests(f *fakeAPI) *Telegram {
	return &Telegram{
		token:   "token",
		baseURL: f.URL,
		http:    f.Client(),
	}
}

func sampleOccurrence() domain.Occurrence {
	due, _ := domain.ParseDay("2026-11-01")
	return domain.Occurrence{
		ID:          42,
		PaymentName: "Аренда офиса",
		Amount:      domain.ParseAmount(45000),
		Currency:    domain.RUB,
		DueDate:     due,
	}
}

// Напоминание уходит в переданный чат с кнопкой, содержащей id периода.
func TestNotifySendsMessageWithButton(t *testing.T) {
	f := newFakeAPI(t)
	tg := newTelegramForTests(f)

	today, _ := domain.ParseDay("2026-10-29")
	if err := tg.Notify("555", sampleOccurrence(), today); err != nil {
		t.Fatalf("отправка: %v", err)
	}

	if len(f.calls) != 1 || f.calls[0] != "sendMessage" {
		t.Fatalf("вызовы: %v", f.calls)
	}

	body := f.bodies[0]
	if body["chat_id"] != "555" {
		t.Errorf("chat_id = %v", body["chat_id"])
	}
	text, _ := body["text"].(string)
	if !strings.Contains(text, "Аренда офиса") || !strings.Contains(text, "45000.00") {
		t.Errorf("текст напоминания: %q", text)
	}

	raw, _ := json.Marshal(body["reply_markup"])
	if !strings.Contains(string(raw), "paid:42") {
		t.Errorf("в кнопке нет идентификатора периода: %s", raw)
	}
	if !strings.Contains(string(raw), "Оплачено") {
		t.Errorf("в кнопке нет подписи: %s", raw)
	}
}

// Разные чаты получают отдельные вызовы sendMessage.
func TestNotifyUsesChatArgument(t *testing.T) {
	f := newFakeAPI(t)
	tg := newTelegramForTests(f)

	today, _ := domain.ParseDay("2026-10-29")
	for _, chat := range []string{"111", "222"} {
		if err := tg.Notify(chat, sampleOccurrence(), today); err != nil {
			t.Fatalf("отправка в %s: %v", chat, err)
		}
	}
	if len(f.bodies) != 2 {
		t.Fatalf("вызовов %d, ожидалось 2", len(f.bodies))
	}
	for i, want := range []string{"111", "222"} {
		if got := f.bodies[i]["chat_id"]; got != want {
			t.Errorf("вызов %d: chat_id = %v, ожидался %s", i, got, want)
		}
	}
}

// SendText отвечает командам тем же методом sendMessage, но без клавиатуры.
func TestSendText(t *testing.T) {
	f := newFakeAPI(t)
	tg := newTelegramForTests(f)

	if err := tg.SendText("555", "Привет"); err != nil {
		t.Fatalf("отправка: %v", err)
	}
	if f.calls[0] != "sendMessage" {
		t.Errorf("метод %s", f.calls[0])
	}
	if f.bodies[0]["text"] != "Привет" || f.bodies[0]["chat_id"] != "555" {
		t.Errorf("тело запроса: %v", f.bodies[0])
	}
	if _, ok := f.bodies[0]["reply_markup"]; ok {
		t.Error("текстовый ответ не должен нести клавиатуру")
	}
}

// Ошибка Telegram API возвращается как ошибка, чтобы напоминание осталось в
// очереди.
func TestNotifyReportsAPIError(t *testing.T) {
	f := newFakeAPI(t)
	f.status = http.StatusBadRequest
	f.reply = `{"ok":false,"description":"chat not found"}`
	tg := newTelegramForTests(f)

	today, _ := domain.ParseDay("2026-10-29")
	err := tg.Notify("555", sampleOccurrence(), today)
	if err == nil {
		t.Fatal("ошибка API не замечена")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("потеряно описание ошибки: %v", err)
	}
}

// Обновления разбираются оба вида: нажатия кнопок и сообщения-команды.
func TestUpdatesParsesCallbacksAndCommands(t *testing.T) {
	f := newFakeAPI(t)
	f.reply = `{"ok":true,"result":[
		{"update_id":10,"callback_query":{"id":"cb1","data":"paid:7",
			"message":{"message_id":3,"chat":{"id":555}}}},
		{"update_id":11,"message":{"text":"/start","chat":{"id":555}}},
		{"update_id":12,"message":{"text":"привет","chat":{"id":555}}},
		{"update_id":13,"message":{"chat":{"id":555}}}
	]}`
	tg := newTelegramForTests(f)

	updates, err := tg.Updates(0)
	if err != nil {
		t.Fatalf("получение: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("обновлений %d, ожидалось 2 (обычное и пустое сообщение игнорируются): %+v",
			len(updates), updates)
	}

	cb := updates[0].Callback
	if cb == nil || updates[0].UpdateID != 10 || cb.Data != "paid:7" || cb.ChatID != "555" || cb.MessageID != 3 {
		t.Errorf("нажатие разобрано неверно: %+v", updates[0])
	}
	cmd := updates[1].Command
	if cmd == nil || updates[1].UpdateID != 11 || cmd.Text != "/start" || cmd.ChatID != "555" {
		t.Errorf("команда разобрана неверно: %+v", updates[1])
	}

	if !strings.Contains(f.queries[0], "timeout=25") {
		t.Errorf("long polling не запрошен: %s", f.queries[0])
	}
	if !strings.Contains(f.queries[0], "offset=0") {
		t.Errorf("offset не передан: %s", f.queries[0])
	}
}

// Ack, ClearButtons и RegisterCommands обращаются к нужным методам API.
func TestAckClearButtonsAndMenu(t *testing.T) {
	f := newFakeAPI(t)
	tg := newTelegramForTests(f)

	if err := tg.Ack("cb1", "Готово"); err != nil {
		t.Fatalf("подтверждение: %v", err)
	}
	if err := tg.ClearButtons("555", 3); err != nil {
		t.Fatalf("снятие клавиатуры: %v", err)
	}
	if err := tg.RegisterCommands(); err != nil {
		t.Fatalf("меню команд: %v", err)
	}

	want := []string{"answerCallbackQuery", "editMessageReplyMarkup", "setMyCommands"}
	if len(f.calls) != len(want) {
		t.Fatalf("вызовы: %v", f.calls)
	}
	for i, w := range want {
		if f.calls[i] != w {
			t.Errorf("вызов %d: %s, ожидался %s", i, f.calls[i], w)
		}
	}
	if f.bodies[0]["text"] != "Готово" {
		t.Errorf("текст подтверждения: %v", f.bodies[0]["text"])
	}

	raw, _ := json.Marshal(f.bodies[2]["commands"])
	for _, name := range []string{"start", "summary", "id", "unsubscribe"} {
		if !strings.Contains(string(raw), name) {
			t.Errorf("в меню нет команды %q: %s", name, raw)
		}
	}
}

// Telegram реализует канал целиком — иначе main не запустит слушателя.
func TestTelegramImplementsChannel(t *testing.T) {
	if _, ok := NewTelegram("token").(Channel); !ok {
		t.Fatal("канал не реализует Channel: слушатель обновлений не запустится")
	}
}
