// Package bot — Telegram-бот Invoice Keeper: напоминания об оплате и команды.
//
// Бот не хранит состояние напоминаний: он спрашивает сервис, о каких периодах
// нужно напомнить, атомарно занимает их через ClaimReminder и отправляет
// сообщения подписавшимся чатам. Защита от дублей живёт в БД, поэтому два
// экземпляра приложения не отправят напоминание дважды.
//
// Для запуска нужен только токен: чат подписывается командой /start, список
// подписчиков хранится в таблице telegram_chats.
package bot

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/aeza/invoice-keeper/internal/app"
	"github.com/aeza/invoice-keeper/internal/domain"
)

// Notifier доставляет сообщение о периоде в конкретный чат.
type Notifier interface {
	Notify(chatID string, o domain.Occurrence, today domain.Day) error
}

// Callback — нажатие inline-кнопки под напоминанием.
type Callback struct {
	UpdateID  int64
	ID        string
	Data      string
	ChatID    string
	MessageID int64
}

// Command — текстовое сообщение, обычно команда вида "/start@bot".
type Command struct {
	UpdateID int64
	ChatID   string
	Text     string
}

// Update — одно обновление Bot API: нажатие кнопки либо сообщение.
type Update struct {
	UpdateID int64
	Callback *Callback
	Command  *Command
}

// Channel — транспорт с обратной связью: отправка, приём и подтверждение.
type Channel interface {
	Notifier
	SendText(chatID, text string) error
	Updates(offset int64) ([]Update, error)
	Ack(callbackID, text string) error
	ClearButtons(chatID string, messageID int64) error
}

// Bot — планировщик напоминаний.
type Bot struct {
	svc      *app.App
	notifier Notifier
	interval time.Duration
}

// New создаёт планировщик. notifier == nil означает «напоминания отключены».
func New(svc *app.App, notifier Notifier, interval time.Duration) *Bot {
	return &Bot{svc: svc, notifier: notifier, interval: interval}
}

// enabled — настроен ли канал. Проверка через nil-интерфейс, а не через
// значение указателя: иначе типизированный nil включил бы отправку.
func (b *Bot) enabled() bool { return b.notifier != nil }

// Run запускает цикл напоминаний до закрытия stop.
//
// Сроки измеряются днями, поэтому частый опрос не нужен — но короткий интервал
// нужен для демонстрации «напоминание сразу» после добавления платежа.
func (b *Bot) Run(stop <-chan struct{}) {
	if !b.enabled() {
		log.Println("telegram: TELEGRAM_BOT_TOKEN не задан — напоминания отключены")
		return
	}

	log.Printf("telegram: напоминания включены, проверка каждые %s", b.interval)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	b.Tick()
	for {
		select {
		case <-ticker.C:
			b.Tick()
		case <-stop:
			return
		}
	}
}

// Tick отправляет все накопившиеся напоминания.
func (b *Bot) Tick() {
	if !b.enabled() {
		return
	}

	chats, err := b.svc.SubscribedChats()
	if err != nil {
		log.Printf("telegram: не удалось получить список чатов: %v", err)
		return
	}
	if len(chats) == 0 {
		log.Println("telegram: нет подписчиков — отправьте боту /start, чтобы получать напоминания")
		return
	}

	pending, err := b.svc.PendingReminders()
	if err != nil {
		log.Printf("telegram: не удалось получить напоминания: %v", err)
		return
	}

	today := b.svc.Today()
	for _, o := range pending {
		claimed, err := b.svc.ClaimReminder(o.ID)
		if err != nil {
			log.Printf("telegram: не удалось занять напоминание %d: %v", o.ID, err)
			continue
		}
		if !claimed {
			continue // уже отправлено другим воркером
		}

		if !b.deliver(chats, o, today) {
			log.Printf("telegram: напоминание %d не доставлено, возвращаю в очередь", o.ID)
			if relErr := b.svc.ReleaseReminder(o.ID); relErr != nil {
				log.Printf("telegram: не удалось вернуть напоминание %d: %v", o.ID, relErr)
			}
			continue
		}
		log.Printf("telegram: напоминание отправлено (%s, %s)", o.PaymentName, o.DueDate)
	}
}

// deliver рассылает период по чатам и сообщает, дошёл ли он хотя бы до одного.
// Требовать успеха от всех нельзя: заблокировавший бота чат иначе возвращал бы
// напоминание в очередь бесконечно.
func (b *Bot) deliver(chats []string, o domain.Occurrence, today domain.Day) bool {
	delivered := false
	for _, chat := range chats {
		if err := b.notifier.Notify(chat, o, today); err != nil {
			log.Printf("telegram: чат %s: %v", chat, err)
			continue
		}
		delivered = true
	}
	return delivered
}

// commandRegistry — необязательная возможность транспорта задать меню команд
// на стороне канала.
type commandRegistry interface{ RegisterCommands() error }

// SetupCommands регистрирует список команд в меню бота, если транспорт это
// умеет. Меню в Telegram — единственное, что видит новый пользователь бота.
func SetupCommands(ch Channel) {
	reg, ok := ch.(commandRegistry)
	if !ok {
		return
	}
	if err := reg.RegisterCommands(); err != nil {
		log.Printf("telegram: не удалось задать меню команд: %v", err)
	}
}

// RunListener слушает обновления Bot API: команды и нажатия кнопок.
//
// Слушатель один на оба типа: getUpdates не допускает параллельных опросов
// одного токена — обновления делились бы между «получателями» и терялись.
func RunListener(svc *app.App, ch Channel, stop <-chan struct{}) {
	if ch == nil {
		return
	}

	// offset = -1 при первом запросе просит только последнее обновление:
	// накопленная за время простоя история команд не переигрывается.
	offset := int64(-1)
	first := true

	for {
		select {
		case <-stop:
			return
		default:
		}

		updates, err := ch.Updates(offset)
		if err != nil {
			log.Printf("telegram: не удалось получить обновления: %v", err)
			if !sleep(stop, 5*time.Second) {
				return
			}
			continue
		}

		if first {
			// Первый ответ мог быть пустым: с этого места начинаем слушать.
			first = false
			if len(updates) == 0 {
				offset = 0
			}
		}

		if len(updates) == 0 {
			// Long polling обычно ждёт сам, но быстрый пустой ответ не должен
			// превращаться в цикл на 100% CPU.
			if !sleep(stop, time.Second) {
				return
			}
			continue
		}

		for _, u := range updates {
			offset = u.UpdateID + 1
			switch {
			case u.Callback != nil:
				handleCallback(svc, ch, *u.Callback)
			case u.Command != nil:
				handleCommand(svc, ch, *u.Command)
			}
		}
	}
}

// sleep ждёт указанное время или до остановки. Возвращает false, если пора
// завершать цикл.
func sleep(stop <-chan struct{}, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-stop:
		return false
	}
}

// handleCallback отмечает период оплаченным по нажатию кнопки. Действие
// разрешено только подписанным чатам: иначе любой, узнавший адрес бота, мог бы
// закрывать чужие платежи.
func handleCallback(svc *app.App, ch Channel, u Callback) {
	id, ok := parsePaidData(u.Data)
	if !ok {
		_ = ch.Ack(u.ID, "Кнопка не распознана")
		return
	}

	subscribed, err := svc.Subscribed(u.ChatID)
	if err != nil {
		log.Printf("telegram: не удалось проверить чат %s: %v", u.ChatID, err)
		return
	}
	if !subscribed {
		log.Printf("telegram: нажатие из неподписанного чата %s проигнорировано", u.ChatID)
		_ = ch.Ack(u.ID, "Нет доступа: отправьте боту /start")
		return
	}

	occurrence, err := svc.Occurrence(id)
	if err != nil {
		log.Printf("telegram: период %d не найден: %v", id, err)
		_ = ch.Ack(u.ID, "Платёж не найден")
		return
	}
	if occurrence.PaidAt != nil {
		_ = ch.Ack(u.ID, "Уже отмечено оплаченным")
		return
	}

	// Фактическая дата оплаты — сегодняшний день в часовом поясе системы.
	if _, err := svc.SetPaid(id, svc.Today().String()); err != nil {
		log.Printf("telegram: не удалось отметить период %d: %v", id, err)
		_ = ch.Ack(u.ID, "Не удалось отметить оплату")
		return
	}

	log.Printf("telegram: период %d отмечен оплаченным из чата %s", id, u.ChatID)
	_ = ch.Ack(u.ID, "Отмечено оплаченным")
	// Убираем кнопку, чтобы повторное нажатие не выглядело как новое действие.
	_ = ch.ClearButtons(u.ChatID, u.MessageID)
}

// handleCommand обслуживает команды бота и отвечает в тот же чат.
func handleCommand(svc *app.App, ch Channel, cmd Command) {
	name, _, _ := strings.Cut(strings.TrimSpace(cmd.Text), " ")
	name, _, _ = strings.Cut(name, "@")

	var reply string
	switch strings.ToLower(name) {
	case "/start":
		if err := svc.SubscribeChat(cmd.ChatID); err != nil {
			log.Printf("telegram: не удалось подписать чат %s: %v", cmd.ChatID, err)
			reply = "Не удалось подписать этот чат, попробуй ещё раз."
			break
		}
		log.Printf("telegram: чат %s подписан на напоминания", cmd.ChatID)
		reply = startText(cmd.ChatID)
	case "/id":
		reply = "ID этого чата: " + cmd.ChatID +
			"\n\nЕго можно указать в .env как TELEGRAM_CHAT_ID — тогда подписка не нужна."
	case "/unsubscribe", "/stop":
		if err := svc.UnsubscribeChat(cmd.ChatID); err != nil {
			reply = "Этот чат и так не получает напоминания."
			break
		}
		reply = "Готово, напоминания сюда больше не приходят. Вернуться — /start."
	case "/summary":
		text, err := summaryText(svc)
		if err != nil {
			log.Printf("telegram: итоги для чата %s: %v", cmd.ChatID, err)
			reply = "Не удалось получить итоги, попробуй позже."
			break
		}
		reply = text
	default:
		reply = helpText()
	}

	if err := ch.SendText(cmd.ChatID, reply); err != nil {
		log.Printf("telegram: не удалось ответить в чат %s: %v", cmd.ChatID, err)
	}
}

func helpText() string {
	return "Invoice Keeper. Команды:\n" +
		"/start — получать напоминания об оплате\n" +
		"/summary — итоги и ближайшие платежи\n" +
		"/id — ID этого чата для TELEGRAM_CHAT_ID\n" +
		"/unsubscribe — отключить напоминания"
}

func startText(chatID string) string {
	return "Готово, напоминания включены.\n\n" +
		"ID этого чата: " + chatID + "\n" +
		"Совет: внеси его в .env как TELEGRAM_CHAT_ID, и подписка перестанет быть нужной.\n\n" +
		helpText()
}

// summaryText — сводка по всем платежам: итоги по валютам и ближайшие сроки.
func summaryText(svc *app.App) (string, error) {
	totals, err := svc.Summary()
	if err != nil {
		return "", err
	}
	upcoming, err := svc.PendingReminders()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Итоги на %s\n", totals.Date)
	for _, c := range totals.ByCurrency {
		fmt.Fprintf(&b, "%s: к оплате %.2f из %.2f\n", c.Currency, c.Outstanding, c.Total)
	}
	fmt.Fprintf(&b, "Периодов: %d · просрочено %d · срок сегодня %d · оплачено %d",
		totals.Status.Total, totals.Status.Overdue, totals.Status.Due, totals.Status.Paid)

	if len(upcoming) == 0 {
		b.WriteString("\n\nБлижайшие напоминания не требуются.")
		return b.String(), nil
	}

	b.WriteString("\n\nБлижайшие:")
	today := svc.Today()
	for i, o := range upcoming {
		if i == 5 {
			fmt.Fprintf(&b, "\n… и ещё %d", len(upcoming)-i)
			break
		}
		fmt.Fprintf(&b, "\n· %s — %s, %s", o.PaymentName, o.Amount.String(o.Currency), o.DueDate)
		switch left := today.DaysUntil(o.DueDate); {
		case left < 0:
			fmt.Fprintf(&b, " (просрочен на %d дн.)", -left)
		case left == 0:
			b.WriteString(" (сегодня)")
		default:
			fmt.Fprintf(&b, " (через %d дн.)", left)
		}
	}
	return b.String(), nil
}

// parsePaidData разбирает callback_data вида "paid:42".
func parsePaidData(data string) (int64, bool) {
	raw, ok := strings.CutPrefix(data, "paid:")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
