package bot

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/app"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/storage"
)

// ── Тестовые двойники ─────────────────────────────────────────────────────

// recorder — тестовый канал доставки: запоминает отправленное и умеет падать.
type recorder struct {
	mu      sync.Mutex
	sent    []string
	failed  int
	allowed []string
}

func newRecorder(chats ...string) *recorder {
	return &recorder{allowed: chats}
}

func (r *recorder) Notify(chatID string, o domain.Occurrence, _ domain.Day) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, allowed := range r.allowed {
		if allowed == chatID {
			r.sent = append(r.sent, o.PaymentName)
			return nil
		}
	}
	r.failed++
	return errors.New("чат недоступен")
}

func (r *recorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

func (r *recorder) failures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failed
}

// fakeChannel — канал с обратной связью: отдаёт заранее заданные обновления и
// закрывает done, когда очередь исчерпана.
type fakeChannel struct {
	*recorder
	updates  [][]Update
	texts    []string
	acks     []string
	cleared  int
	callAt   int
	done     chan struct{}
	once     sync.Once
	commands int
}

func newFakeChannel(updates ...[]Update) *fakeChannel {
	return &fakeChannel{recorder: newRecorder(), updates: updates, done: make(chan struct{})}
}

func (f *fakeChannel) SendText(chatID, text string) error {
	f.texts = append(f.texts, chatID+": "+text)
	return nil
}

func (f *fakeChannel) Updates(int64) ([]Update, error) {
	if f.callAt >= len(f.updates) {
		f.once.Do(func() { close(f.done) })
		return nil, nil
	}
	batch := f.updates[f.callAt]
	f.callAt++
	return batch, nil
}

func (f *fakeChannel) Ack(_, text string) error {
	f.acks = append(f.acks, text)
	return nil
}

func (f *fakeChannel) ClearButtons(string, int64) error {
	f.cleared++
	return nil
}

func callbackUpdate(id int64, data, chatID string, messageID int64) []Update {
	return []Update{{UpdateID: id, Callback: &Callback{
		UpdateID: id, ID: "cb" + itoa(id), Data: data, ChatID: chatID, MessageID: messageID,
	}}}
}

func commandUpdate(id int64, text, chatID string) []Update {
	return []Update{{UpdateID: id, Command: &Command{UpdateID: id, Text: text, ChatID: chatID}}}
}

// runUntilDrained запускает слушателя, ждёт разбора всех пачек и останавливает.
func runUntilDrained(t *testing.T, svc *app.App, ch *fakeChannel) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		RunListener(svc, ch, stop)
		close(done)
	}()

	select {
	case <-ch.done:
	case <-time.After(3 * time.Second):
		t.Fatal("слушатель не разобрал очередь обновлений")
	}
	close(stop)
	<-done
}

func newService(t *testing.T) *app.App {
	t.Helper()

	store, err := storage.New(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	svc, err := app.New(store, "Europe/Moscow")
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	if err := svc.Bootstrap("admin", "admin123"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return svc
}

// subscribe подписывает чат — напоминания уходят только подписчикам.
func subscribe(t *testing.T, svc *app.App, chats ...string) {
	t.Helper()
	for _, c := range chats {
		if err := svc.SubscribeChat(c); err != nil {
			t.Fatalf("подписка %s: %v", c, err)
		}
	}
}

// paymentWithDueSoon создаёт платёж и возвращает его первый период.
func paymentWithDueSoon(t *testing.T, svc *app.App, name string) domain.Occurrence {
	t.Helper()
	today := svc.Today()

	p, err := svc.CreatePayment(app.PaymentInput{
		Name: name, Amount: 45000, Currency: domain.RUB,
		Period: domain.Monthly, DueDate: today.AddDays(2).String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := svc.Occurrences(p.ID)
	if err != nil || len(list) == 0 {
		t.Fatalf("периоды: %v", err)
	}
	return list[0]
}

// ── Напоминания ───────────────────────────────────────────────────────────

// Напоминание уходит один раз: повторный тик не создаёт дубль.
func TestTickSendsEachReminderOnce(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555")
	paymentWithDueSoon(t, svc, "Аренда")

	rec := newRecorder("555")
	scheduler := New(svc, rec, time.Minute)

	scheduler.Tick()
	scheduler.Tick()

	if got := rec.messages(); len(got) != 1 {
		t.Fatalf("ожидалось одно сообщение, получено %d: %v", len(got), got)
	}
}

// Напоминание уходит каждому подписчику.
func TestTickNotifiesEverySubscriber(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555", "777")
	paymentWithDueSoon(t, svc, "Аренда")

	rec := newRecorder("555", "777")
	New(svc, rec, time.Minute).Tick()

	if got := rec.messages(); len(got) != 2 {
		t.Fatalf("сообщений %d, ожидалось 2 (по одному на чат): %v", len(got), got)
	}
}

// Подписки берутся из базы и переживают перезапуск: новый планировщик с тем же
// хранилищем видит те же чаты.
func TestSubscribersArePersisted(t *testing.T) {
	store, err := storage.New(filepath.Join(t.TempDir(), "persist.db"))
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	defer store.Close()

	if err := store.AddTelegramChat("555"); err != nil {
		t.Fatal(err)
	}

	svc, err := app.New(store, "Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Bootstrap("admin", "admin123"); err != nil {
		t.Fatal(err)
	}
	paymentWithDueSoon(t, svc, "Аренда")

	rec := newRecorder("555")
	New(svc, rec, time.Minute).Tick()

	if got := rec.messages(); len(got) != 1 {
		t.Fatalf("сообщений %d, ожидалось 1: %v", len(got), got)
	}
}

// Без подписчиков напоминания никуда не уходят и не помечаются отправленными:
// они дождутся первого /start.
func TestTickWithoutSubscribersKeepsQueue(t *testing.T) {
	svc := newService(t)
	o := paymentWithDueSoon(t, svc, "Аренда")

	New(svc, newRecorder(), time.Minute).Tick()

	after, err := svc.Occurrence(o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ReminderSent {
		t.Fatal("напоминание помечено отправленным без доставки")
	}
}

// Если доставку не удалось ни в один чат, напоминание возвращается в очередь.
func TestFailedDeliveryIsRetried(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555")
	o := paymentWithDueSoon(t, svc, "Сервер")

	blocked := newRecorder() // ни один чат не проходит
	scheduler := New(svc, blocked, time.Minute)

	scheduler.Tick()
	if got := blocked.messages(); len(got) != 0 {
		t.Fatalf("при ошибке доставки сообщений быть не должно: %v", got)
	}

	after, err := svc.Occurrence(o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ReminderSent {
		t.Fatal("напоминание потеряно: флаг отправленного снят не был")
	}
}

// Частичная доставка засчитывается: заблокировавший бота чат не должен
// бесконечно возвращать напоминание в очередь.
func TestPartialDeliveryCounts(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555", "888")
	o := paymentWithDueSoon(t, svc, "Аренда")

	rec := newRecorder("555") // чат 888 недоступен
	New(svc, rec, time.Minute).Tick()

	if rec.failures() != 1 {
		t.Errorf("неудачных доставок %d, ожидался 1", rec.failures())
	}
	after, err := svc.Occurrence(o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ReminderSent {
		t.Error("напоминание вернулось в очередь, хотя один чат его получил")
	}
}

// Оплаченный период не напоминает.
func TestPaidOccurrenceIsSilent(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555")
	o := paymentWithDueSoon(t, svc, "Подписка")

	if _, err := svc.SetPaid(o.ID, svc.Today().String()); err != nil {
		t.Fatal(err)
	}

	rec := newRecorder("555")
	New(svc, rec, time.Minute).Tick()

	if got := rec.messages(); len(got) != 0 {
		t.Fatalf("оплаченный период напомнил о себе: %v", got)
	}
}

// Без настроенного канала планировщик не падает и ничего не отправляет.
func TestDisabledNotifierIsSafe(t *testing.T) {
	svc := newService(t)
	paymentWithDueSoon(t, svc, "Тест")

	New(svc, nil, time.Minute).Tick() // не должно паниковать
	RunListener(svc, nil, closedChan())
}

// NewTelegram возвращает nil-интерфейс без токена — приложение работает без
// бота. Проверяется именно интерфейс: типизированный nil-указатель в нём не
// равен nil и сломал бы проверку в планировщике.
func TestTelegramRequiresToken(t *testing.T) {
	if NewTelegram("") != nil {
		t.Fatal("без токена отправитель создаваться не должен")
	}
	if NewTelegram("token") == nil {
		t.Fatal("с токеном отправитель должен создаваться")
	}
}

// Текст напоминания содержит название, сумму, валюту и дату — требование кейса.
func TestReminderTextContent(t *testing.T) {
	due, _ := domain.ParseDay("2026-11-01")
	today, _ := domain.ParseDay("2026-10-29")

	o := domain.Occurrence{
		PaymentName: "Аренда офиса",
		Amount:      domain.ParseAmount(45000),
		Currency:    domain.RUB,
		DueDate:     due,
	}

	text := app.ReminderText(o, today)
	for _, want := range []string{"Аренда офиса", "45000.00", "₽", "2026-11-01"} {
		if !strings.Contains(text, want) {
			t.Errorf("в сообщении нет %q:\n%s", want, text)
		}
	}
}

// ── Кнопка «Оплачено» ─────────────────────────────────────────────────────

// Нажатие кнопки из подписанного чата отмечает период оплаченным.
func TestCallbackMarksPaid(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555")
	o := paymentWithDueSoon(t, svc, "Аренда")

	ch := newFakeChannel(callbackUpdate(1, "paid:"+itoa(o.ID), "555", 10))
	runUntilDrained(t, svc, ch)

	paid, err := svc.Occurrence(o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paid.PaidAt == nil {
		t.Fatal("период не отмечен оплаченным")
	}
	if paid.PaidAt.String() != svc.Today().String() {
		t.Errorf("дата оплаты %s, ожидалась %s", paid.PaidAt, svc.Today())
	}
	if len(ch.acks) == 0 || ch.acks[0] != "Отмечено оплаченным" {
		t.Errorf("неожиданное подтверждение: %v", ch.acks)
	}
	if ch.cleared != 1 {
		t.Errorf("клавиатура убрана %d раз, ожидался 1", ch.cleared)
	}
}

// Нажатие из неподписанного чата не меняет данные.
func TestCallbackFromUnsubscribedChatIsRejected(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555")
	o := paymentWithDueSoon(t, svc, "Аренда")

	ch := newFakeChannel(callbackUpdate(1, "paid:"+itoa(o.ID), "999", 10))
	runUntilDrained(t, svc, ch)

	after, _ := svc.Occurrence(o.ID)
	if after.PaidAt != nil {
		t.Fatal("чужой чат смог отметить оплату")
	}
	if len(ch.acks) == 0 || !strings.Contains(ch.acks[0], "Нет доступа") {
		t.Errorf("неожиданное подтверждение: %v", ch.acks)
	}
}

// Повторное нажатие не переписывает уже сохранённую дату оплаты.
func TestCallbackIsIdempotent(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555")
	o := paymentWithDueSoon(t, svc, "Аренда")

	first := svc.Today().AddDays(-1).String()
	if _, err := svc.SetPaid(o.ID, first); err != nil {
		t.Fatal(err)
	}

	ch := newFakeChannel(callbackUpdate(1, "paid:"+itoa(o.ID), "555", 10))
	runUntilDrained(t, svc, ch)

	after, _ := svc.Occurrence(o.ID)
	if after.PaidAt == nil || after.PaidAt.String() != first {
		t.Fatalf("дата оплаты переписана: %v", after.PaidAt)
	}
	if len(ch.acks) == 0 || ch.acks[0] != "Уже отмечено оплаченным" {
		t.Errorf("неожиданное подтверждение: %v", ch.acks)
	}
}

// Некорректные данные кнопки не приводят к изменениям.
func TestCallbackWithBadData(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555")

	for _, data := range []string{"", "paid:", "paid:abc", "paid:0", "other:1"} {
		ch := newFakeChannel(callbackUpdate(1, data, "555", 1))
		runUntilDrained(t, svc, ch)
		if len(ch.acks) == 0 {
			t.Errorf("на %q не было подтверждения", data)
		}
	}
}

// Нажатие по несуществующему периоду не роняет слушателя.
func TestCallbackForMissingOccurrence(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555")

	ch := newFakeChannel(callbackUpdate(1, "paid:999999", "555", 1))
	runUntilDrained(t, svc, ch)

	if len(ch.acks) == 0 || ch.acks[0] != "Платёж не найден" {
		t.Errorf("неожиданное подтверждение: %v", ch.acks)
	}
}

// ── Команды ───────────────────────────────────────────────────────────────

// /start подписывает чат, и следующее напоминание в него уходит.
func TestStartSubscribesChat(t *testing.T) {
	svc := newService(t)

	ch := newFakeChannel(commandUpdate(1, "/start", "555"))
	runUntilDrained(t, svc, ch)

	if !strings.Contains(strings.Join(ch.texts, "\n"), "напоминания включены") {
		t.Errorf("ответ на /start: %v", ch.texts)
	}
	subscribed, err := svc.Subscribed("555")
	if err != nil || !subscribed {
		t.Fatalf("чат не подписан: %v %v", subscribed, err)
	}

	rec := newRecorder("555")
	New(svc, rec, time.Minute).Tick()
	if got := rec.messages(); len(got) != 0 {
		t.Errorf("без платежей отправлять нечего: %v", got)
	}

	paymentWithDueSoon(t, svc, "Аренда")
	New(svc, rec, time.Minute).Tick()
	if got := rec.messages(); len(got) != 1 {
		t.Errorf("после /start напоминаний %d, ожидался 1: %v", len(got), got)
	}
}

// Команда с упоминанием бота (/start@my_bot) работает так же.
func TestCommandWithMention(t *testing.T) {
	svc := newService(t)

	ch := newFakeChannel(commandUpdate(1, "/start@invoice_keeper_bot", "555"))
	runUntilDrained(t, svc, ch)

	if subscribed, _ := svc.Subscribed("555"); !subscribed {
		t.Error("команда с упоминанием не подписала чат")
	}
}

// /id сообщает идентификатор чата — его подставляют в TELEGRAM_CHAT_ID.
func TestIdCommandAnswersChatID(t *testing.T) {
	svc := newService(t)

	ch := newFakeChannel(commandUpdate(1, "/id", "-1001234567890"))
	runUntilDrained(t, svc, ch)

	if len(ch.texts) != 1 || !strings.Contains(ch.texts[0], "-1001234567890") {
		t.Errorf("ответ на /id: %v", ch.texts)
	}
}

// /unsubscribe отключает напоминания для чата.
func TestUnsubscribeStopsReminders(t *testing.T) {
	svc := newService(t)
	subscribe(t, svc, "555")
	paymentWithDueSoon(t, svc, "Аренда")

	ch := newFakeChannel(commandUpdate(1, "/unsubscribe", "555"))
	runUntilDrained(t, svc, ch)

	if subscribed, _ := svc.Subscribed("555"); subscribed {
		t.Error("чат остался подписан")
	}

	rec := newRecorder("555")
	New(svc, rec, time.Minute).Tick()
	if got := rec.messages(); len(got) != 0 {
		t.Errorf("после отписки напоминания пришли: %v", got)
	}
}

// /summary отвечает итогами и содержит название платежа.
func TestSummaryCommand(t *testing.T) {
	svc := newService(t)
	paymentWithDueSoon(t, svc, "Аренда офиса")

	ch := newFakeChannel(commandUpdate(1, "/summary", "555"))
	runUntilDrained(t, svc, ch)

	if len(ch.texts) != 1 {
		t.Fatalf("ответов %d, ожидался 1", len(ch.texts))
	}
	for _, want := range []string{"Итоги на", "Аренда офиса", "RUB"} {
		if !strings.Contains(ch.texts[0], want) {
			t.Errorf("в ответе нет %q:\n%s", want, ch.texts[0])
		}
	}
}

// Неизвестная команда не ломает бота и подсказывает список команд.
func TestUnknownCommandFallsBackToHelp(t *testing.T) {
	svc := newService(t)

	ch := newFakeChannel(commandUpdate(1, "/wat", "555"))
	runUntilDrained(t, svc, ch)

	if len(ch.texts) == 0 || !strings.Contains(ch.texts[0], "/start") {
		t.Errorf("подсказка не выдана: %v", ch.texts)
	}
}

// SetupCommands не паникует на транспорте без поддержки меню.
func TestSetupCommandsTolerant(t *testing.T) {
	SetupCommands(newFakeChannel())
}

// parsePaidData разбирает только корректный формат.
func TestParsePaidData(t *testing.T) {
	cases := map[string]int64{"paid:1": 1, "paid:42": 42}
	for in, want := range cases {
		got, ok := parsePaidData(in)
		if !ok || got != want {
			t.Errorf("%q → %d, %v; ожидалось %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "paid:", "paid:-1", "paid:x", "1"} {
		if _, ok := parsePaidData(in); ok {
			t.Errorf("%q не должен разбираться", in)
		}
	}
}

// closedChan — уже закрытый канал: цикл обработки завершится сразу.
func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
