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

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/app"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
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
// Tick отправляет персональные напоминания подписанным чатам.
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

	today := b.svc.Today()

	// Проходим по каждому подписанному чату персонально
	for _, chatID := range chats {
		// Находим пользователя, привязанного к этому Telegram-чату
		user, err := b.svc.UserByChat(chatID)
		if err != nil {
			// Если чат не привязан ни к какому пользователю, пропускаем его
			continue
		}

		// Получаем напоминания СТРОГО для этого пользователя
		pending, err := b.svc.PendingRemindersForUser(user.ID)
		if err != nil {
			log.Printf("telegram: не удалось получить напоминания для пользователя %d (чат %s): %v", user.ID, chatID, err)
			continue
		}

		for _, o := range pending {
			claimed, err := b.svc.ClaimReminder(o.ID)
			if err != nil {
				log.Printf("telegram: не удалось занять напоминание %d: %v", o.ID, err)
				continue
			}
			if !claimed {
				continue // уже отправлено другим воркером или процессом
			}

			// Доставляем напоминание СТРОГО в этот конкретный чат
			if err := b.notifier.Notify(chatID, o, today); err != nil {
				log.Printf("telegram: чат %s не принял напоминание %d: %v", chatID, o.ID, err)
				// Возвращаем в очередь, если доставка не удалась
				if relErr := b.svc.ReleaseReminder(o.ID); relErr != nil {
					log.Printf("telegram: не удалось вернуть напоминание %d: %v", o.ID, relErr)
				}
				continue
			}
			log.Printf("telegram: персональное напоминание отправлено в чат %s (%s, %s)", chatID, o.PaymentName, o.DueDate)
		}
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
	id, isUnpay, ok := parseActionData(u.Data)
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

	// Если это запрос на отмену оплаты (unpay:)
	if isUnpay {
		if occurrence.PaidAt == nil {
			_ = ch.Ack(u.ID, "Платёж уже не оплачен")
			return
		}
		// Передаем пустую строку, чтобы снять отметку и очистить дату в БД
		if _, err := svc.SetPaid(id, ""); err != nil {
			log.Printf("telegram: не удалось отменить оплату периода %d: %v", id, err)
			_ = ch.Ack(u.ID, "Не удалось отменить оплату")
			return
		}

		log.Printf("telegram: оплата периода %d отменена из чата %s", id, u.ChatID)
		_ = ch.Ack(u.ID, "Оплата отменена")
		_ = ch.ClearButtons(u.ChatID, u.MessageID)
		return
	}

	// Штатная логика подтверждения оплаты (paid:)
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
	// Убираем клавиатуру, чтобы повторное нажатие не выглядело как новое действие.
	_ = ch.ClearButtons(u.ChatID, u.MessageID)
}

// handleCommand обслуживает команды бота и отвечает в тот же чат.
func handleCommand(svc *app.App, ch Channel, cmd Command) {
	name, _, _ := strings.Cut(strings.TrimSpace(cmd.Text), " ")
	name, _, _ = strings.Cut(name, "@")

	var reply string
	var err error
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
	case "/payments":
		// Поддерживаем фильтр по месяцу, например: /payments 2026-11
		_, monthArg, _ := strings.Cut(strings.TrimSpace(cmd.Text), " ")
		var err error
		reply, err = userPaymentsText(svc, cmd.ChatID, strings.TrimSpace(monthArg))
		if err != nil {
			log.Printf("telegram: ошибка получения платежей для чата %s: %v", cmd.ChatID, err)
			reply = "Не удалось загрузить список платежей."
		}
	case "/pay":
		err = sendUnpaidPaymentsList(svc, ch, cmd.ChatID)
		if err != nil {
			log.Printf("telegram: ошибка отправки списка для оплаты в чат %s: %v", cmd.ChatID, err)
			_ = ch.SendText(cmd.ChatID, "Не удалось загрузить список неоплаченных платежей.")
		}
	case "/overdue":
		var err error
		reply, err = userOverdueText(svc, cmd.ChatID)
		if err != nil {
			log.Printf("telegram: ошибка получения просрочек для чата %s: %v", cmd.ChatID, err)
			reply = "Не удалось загрузить список просрочек."
		}
	case "/unpay":
		err = sendUnpaidListForCancel(svc, ch, cmd.ChatID)
		if err != nil {
			log.Printf("telegram: ошибка отправки списка для отмены в чат %s: %v", cmd.ChatID, err)
			_ = ch.SendText(cmd.ChatID, "Не удалось загрузить список оплаченных платежей.")
		}
		return
	case "/setdate":
		reply, err = handleSetDateCommand(svc, cmd.ChatID, cmd.Text)
		if err != nil {
			log.Printf("telegram: ошибка смены даты для чата %s: %v", cmd.ChatID, err)
		}
	case "/budget":
		reply, err = handleBudgetCommand(svc, cmd.ChatID, cmd.Text)
		if err != nil {
			log.Printf("telegram: ошибка расчета бюджета для чата %s: %v", cmd.ChatID, err)
			reply = "Не удалось рассчитать бюджет."
		}
	case "/calendar", "/cal":
		reply, err = handleCalendarCommand(svc, cmd.ChatID, cmd.Text)
		if err != nil {
			log.Printf("telegram: ошибка генерации календаря для чата %s: %v", cmd.ChatID, err)
			reply = "Не удалось сформировать календарь оплат."
		}

	case "/summary":
		_, dateArg, _ := strings.Cut(strings.TrimSpace(cmd.Text), " ")
		text, err := summaryText(svc, dateArg)
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
	return "🤖 Доступные команды Invoice Keeper:\n\n" +
		"📊 Информация и отчеты:\n" +
		"• /summary — Общие итоги и ближайшие платежи\n" +
		"• /payments — Список ваших платежей (можно указать месяц, например: <code>/payments 2026-11</code>)\n" +
		"• /overdue — Ваши текущие просрочки по оплате\n\n" +
		"💳 Управление платежами:\n" +
		"• /pay — Выбрать неоплаченный платеж и отметить его оплаченным через кнопки\n" +
		"• /unpay — Отменить отметку об оплате (снять статус)\n" +
		"• /setdate <code>ID дата</code> — Указать или изменить фактическую дату оплаты (например: <code>/setdate 15 2026-10-24</code>)\n\n" +
		"⚙️ Настройка и чат:\n" +
		"• /start — Подписаться на автоматические напоминания\n" +
		"• /unsubscribe — Отписаться от напоминаний\n" +
		"• /id — Узнать Telegram ID текущего чата\n" +
		"• /help — Показать эту справку"
}

// userPaymentsText формирует список платежей пользователя с фильтрацией по месяцу (формат YYYY-MM).
func userPaymentsText(svc *app.App, chatID string, monthFilter string) (string, error) {
	user, err := svc.UserByChat(chatID)
	if err != nil {
		return "⚠️ Ваш Telegram чат не привязан ни к одному пользователю в системе. Обратитесь к администратору.", nil
	}

	// Получаем платежи, назначенные этому пользователю
	occurrences, err := svc.OccurrencesFiltered(0, user.ID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if monthFilter != "" {
		fmt.Fprintf(&b, "📋 Платежи за период: %s\n\n", monthFilter)
	} else {
		b.WriteString("📋 Ваши активные и будущие платежи:\n\n")
	}

	count := 0

	for _, o := range occurrences {
		// Если задан фильтр по месяцу (например, "2026-11"), проверяем вхождение в строку даты
		if monthFilter != "" && !strings.HasPrefix(o.DueDate.String(), monthFilter) {
			continue
		}

		statusIcon := "⏳"
		if o.PaidAt != nil {
			statusIcon = "✅"
		} else if o.Status == domain.StatusOverdue {
			statusIcon = "🔴"
		}

		// Выводим название, сумму и ID периода в тегах тега code для удобного копирования
		fmt.Fprintf(&b, "%s %s [ID: %d] — %s\n📅 Срок: %s",
			statusIcon, o.PaymentName, o.ID, o.Amount.String(o.Currency), o.DueDate)

		if o.PaidAt != nil {
			fmt.Fprintf(&b, " (Оплачено: %s)", o.PaidAt)
		}
		b.WriteString("\n\n")
		count++
	}

	if count == 0 {
		return "📭 По вашему запросу платежей не найдено.", nil
	}

	return b.String(), nil
}

// userOverdueText выводит список просроченных платежей пользователя.
func userOverdueText(svc *app.App, chatID string) (string, error) {
	user, err := svc.UserByChat(chatID)
	if err != nil {
		return "⚠️ Ваш Telegram чат не привязан к учетной записи в системе.", nil
	}

	occurrences, err := svc.OccurrencesFiltered(0, user.ID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("🔴 Ваши просроченные задолженности:\n\n")

	today := svc.Today()
	count := 0

	for _, o := range occurrences {
		// Фильтруем только просроченные и неоплаченные
		if o.Status == domain.StatusOverdue && o.PaidAt == nil {
			daysLate := today.DaysUntil(o.DueDate) // будет отрицательным числом
			fmt.Fprintf(&b, "📌 %s\n💰 Сумма: %s\n⏰ Просрочено на: %d дн. (срок был %s)\n\n",
				o.PaymentName, o.Amount.String(o.Currency), -daysLate, o.DueDate)
			count++
		}
	}

	if count == 0 {
		return "🎉 У вас нет просроченных платежей. Отличная финансовая дисциплина!", nil
	}

	return b.String(), nil
}

// sendUnpaidPaymentsList находит все неоплаченные периоды пользователя и отправляет их с кнопками оплаты.
// sendUnpaidPaymentsList формирует одно сообщение со списком неоплаченных платежей и кнопками для каждого.
func sendUnpaidPaymentsList(svc *app.App, ch Channel, chatID string) error {
	user, err := svc.UserByChat(chatID)
	if err != nil {
		return ch.SendText(chatID, "⚠️ Ваш Telegram чат не привязан ни к одному пользователю в системе.")
	}

	occurrences, err := svc.OccurrencesFiltered(0, user.ID)
	if err != nil {
		return err
	}

	var rows [][]map[string]string
	var b strings.Builder
	b.WriteString("💳 Выберите платеж для оплаты:\n\n")

	count := 0
	for _, o := range occurrences {
		if o.PaidAt != nil {
			continue
		}

		count++
		// Текст в чате с общей сводкой
		fmt.Fprintf(&b, "%d. %s — %s (Срок: %s)\n",
			count, o.PaymentName, o.Amount.String(o.Currency), o.DueDate)

		// ИСПРАВЛЕНО: Ключ для данных кнопки в Telegram API строго называется "callback_data"
		rows = append(rows, []map[string]string{
			{
				"text":          fmt.Sprintf("✅ Оплатить: %s (%s)", o.PaymentName, o.Amount.String(o.Currency)),
				"callback_data": "paid:" + strconv.FormatInt(o.ID, 10),
			},
		})

		// Ограничим выборку первыми 5 платежами
		if count >= 5 {
			b.WriteString("\n… показаны первые 5 неоплаченных платежей.")
			break
		}
	}

	if count == 0 {
		return ch.SendText(chatID, "🎉 У вас нет неоплаченных платежей!")
	}

	// Отправляем ОДНО сообщение со всеми кнопками
	if tg, ok := ch.(*Telegram); ok {
		return tg.call("sendMessage", map[string]any{
			"chat_id":    chatID,
			"text":       b.String(),
			"parse_mode": "HTML",
			"reply_markup": map[string]any{
				"inline_keyboard": rows,
			},
		}, nil)
	}

	// Запасной вариант для тестов
	return ch.SendText(chatID, b.String())
}

// sendPaymentButton отправляет сообщение с инлайн-кнопкой «Оплачено» для конкретного периода.
func sendPaymentButton(ch Channel, chatID string, text string, occurrenceID int64) error {
	// Поскольку стандартный интерфейс Channel содержит только SendText (без кастомных payloads),
	// мы можем привести интерфейс к структуре Telegram, если используется реальный бот,
	// либо расширить интерфейс Channel. Если вы работаете в рамках текущего пакета bot,
	// можно вызвать метод напрямую через структуру Telegram или отправить через вспомогательный метод.

	// Если у вас доступна структура Telegram, можно использовать ее метод вызова API:
	if tg, ok := ch.(*Telegram); ok {
		return tg.call("sendMessage", map[string]any{
			"chat_id":    chatID,
			"text":       text,
			"parse_mode": "HTML",
			"reply_markup": map[string]any{
				"inline_keyboard": [][]map[string]string{{
					{"text": "✅ Отметить оплаченным", "callback_data": "paid:" + strconv.FormatInt(occurrenceID, 10)},
				}},
			},
		}, nil)
	}

	// Запасной вариант через стандартный SendText, если тип канала — тестировщик (recorder/fakeChannel)
	return ch.SendText(chatID, text+" [Кнопка оплаты]")
}

func startText(chatID string) string {
	return "Готово, напоминания включены.\n\n" +
		"ID этого чата: " + chatID + "\n" +
		"Совет: внеси его в .env как TELEGRAM_CHAT_ID, и подписка перестанет быть нужной.\n\n" +
		helpText()
}

// summaryText — сводка по всем платежам: итоги по валютам и ближайшие сроки.
// summaryText — сводка по всем платежам: итоги по валютам и ближайшие сроки.
func summaryText(svc *app.App, dateArg string) (string, error) {
	targetDate := strings.TrimSpace(dateArg)

	var totals *app.Totals
	var err error

	// Если дата передана, считаем итоги на эту дату, иначе — общие итоги как раньше
	if targetDate != "" {
		totals, err = svc.SummaryOnDate(targetDate)
	} else {
		totals, err = svc.Summary()
	}
	if err != nil {
		return "", err
	}

	upcoming, err := svc.PendingReminders()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("📊 Сводка по финансам и платежам\n")
	fmt.Fprintf(&b, "📅 Актуально на: %s\n", totals.Date)
	b.WriteString("────────────────────────\n\n")

	// Блок сумм по валютам
	b.WriteString("💰 По валютам:\n")
	if len(totals.ByCurrency) == 0 {
		b.WriteString("• Нет активных валют\n")
	} else {
		for _, c := range totals.ByCurrency {
			fmt.Fprintf(&b, "• %s: к оплате %.2f из %.2f\n", c.Currency, c.Outstanding, c.Total)
		}
	}
	b.WriteString("\n")

	// Блок статистики по статусам
	b.WriteString("📌 Состояние периодов:\n")
	fmt.Fprintf(&b, "• Всего: %d\n", totals.Status.Total)
	fmt.Fprintf(&b, "• 🔴 Просрочено: %d\n", totals.Status.Overdue)
	fmt.Fprintf(&b, "• ⏳ Срок сегодня: %d\n", totals.Status.Due)
	fmt.Fprintf(&b, "• ✅ Оплачено: %d\n\n", totals.Status.Paid)

	// Блок ближайших напоминаний
	if len(upcoming) == 0 {
		b.WriteString("🎉 Ближайшие напоминания не требуются.")
		return b.String(), nil
	}

	b.WriteString("⏰ Ближайшие платежи:")
	today := svc.Today()
	count := 0
	for _, o := range upcoming {
		if count >= 5 {
			fmt.Fprintf(&b, "\n\n<i>… и ещё %d платежей</i>", len(upcoming)-count)
			break
		}
		count++

		statusIcon := "⏳"
		if o.Status == domain.StatusOverdue {
			statusIcon = "🔴"
		}

		fmt.Fprintf(&b, "\n%s %s — %s\n   📅 Срок: %s",
			statusIcon, o.PaymentName, o.Amount.String(o.Currency), o.DueDate)

		switch left := today.DaysUntil(o.DueDate); {
		case left < 0:
			fmt.Fprintf(&b, " (⚠️ просрочен на %d дн.)", -left)
		case left == 0:
			b.WriteString(" (🔥 сегодня!)")
		default:
			fmt.Fprintf(&b, " (через %d дн.)", left)
		}
	}

	return b.String(), nil
}

// parseActionData разбирает "paid:42" или "unpay:42".
func parseActionData(data string) (int64, bool, bool) {
	if raw, ok := strings.CutPrefix(data, "paid:"); ok {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && id > 0 {
			return id, false, true
		}
	}
	if raw, ok := strings.CutPrefix(data, "unpay:"); ok {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && id > 0 {
			return id, true, true
		}
	}
	return 0, false, false
}

// sendUnpaidListForCancel выводит список уже оплаченных платежей с кнопкой отмены.
func sendUnpaidListForCancel(svc *app.App, ch Channel, chatID string) error {
	user, err := svc.UserByChat(chatID)
	if err != nil {
		return ch.SendText(chatID, "⚠️ Ваш Telegram чат не привязан ни к одному пользователю в системе.")
	}

	occurrences, err := svc.OccurrencesFiltered(0, user.ID)
	if err != nil {
		return err
	}

	var rows [][]map[string]string
	var b strings.Builder
	b.WriteString("↩️ Выберите платеж для отмены оплаты:\n\n")

	count := 0
	for _, o := range occurrences {
		if o.PaidAt == nil {
			continue // Показываем только те, которые оплачены
		}

		count++
		fmt.Fprintf(&b, "%d. %s — %s (Оплачено: %s)\n",
			count, o.PaymentName, o.Amount.String(o.Currency), o.PaidAt)

		rows = append(rows, []map[string]string{
			{
				"text":          fmt.Sprintf("❌ Отменить оплату: %s", o.PaymentName),
				"callback_data": "unpay:" + strconv.FormatInt(o.ID, 10),
			},
		})

		if count >= 5 {
			break
		}
	}

	if count == 0 {
		return ch.SendText(chatID, "📭 У вас нет оплаченных платежей, отметку которых можно снять.")
	}

	if tg, ok := ch.(*Telegram); ok {
		return tg.call("sendMessage", map[string]any{
			"chat_id":    chatID,
			"text":       b.String(),
			"parse_mode": "HTML",
			"reply_markup": map[string]any{
				"inline_keyboard": rows,
			},
		}, nil)
	}

	return ch.SendText(chatID, b.String())
}

// handleCalendarCommand выводит расписание платежей по дням на выбранный месяц.
func handleCalendarCommand(svc *app.App, chatID string, text string) (string, error) {
	user, err := svc.UserByChat(chatID)
	if err != nil {
		return "⚠️ Ваш Telegram чат не привязан к учетной записи.", nil
	}

	today := svc.Today()
	var year, month int
	_, _ = fmt.Sscanf(today.String(), "%d-%d", &year, &month)
	if year == 0 {
		year = 2026
		month = 10
	}

	// Проверяем, передал ли пользователь конкретный месяц в аргументе, напр: /calendar 2026-11
	args := strings.Fields(text)
	if len(args) > 1 {
		arg := args[1]
		var y, m int
		if _, parseErr := fmt.Sscanf(arg, "%d-%d", &y, &m); parseErr == nil && m >= 1 && m <= 12 {
			year = y
			month = m
		}
	}

	calendarDays, periodStr, err := svc.MonthlyCalendar(user.ID, year, month)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📅 Календарь оплат на %s\n", periodStr)
	b.WriteString("────────────────────────\n\n")

	if len(calendarDays) == 0 {
		return fmt.Sprintf("📭 На %s плановых платежей не найдено.", periodStr), nil
	}

	for _, day := range calendarDays {
		fmt.Fprintf(&b, "📆 %s\n", day.Date)
		for _, o := range day.Items {
			statusIcon := "⏳"
			if o.PaidAt != nil {
				statusIcon = "✅"
			} else if o.Status == domain.StatusOverdue {
				statusIcon = "🔴"
			}

			fmt.Fprintf(&b, "  %s %s — %s [ID: %d]\n",
				statusIcon, o.PaymentName, o.Amount.String(o.Currency), o.ID)
		}
		b.WriteString("\n")
	}

	b.WriteString("💡 Совет: Чтобы посмотреть другой месяц, укажите его, например:/calendar 2026-11")
	return b.String(), nil
}

// handleBudgetCommand формирует отчет по плановым расходам на месяц или год.
func handleBudgetCommand(svc *app.App, chatID string, text string) (string, error) {
	user, err := svc.UserByChat(chatID)
	if err != nil {
		return "⚠️ Ваш Telegram чат не привязан к учетной записи.", nil
	}

	today := svc.Today()
	// Парсим год и месяц из строкового представления даты (YYYY-MM-DD)
	var year, month int
	_, _ = fmt.Sscanf(today.String(), "%d-%d", &year, &month)
	if year == 0 {
		year = 2026
		month = 10
	}

	args := strings.Fields(text)
	if len(args) > 1 {
		arg := strings.ToLower(args[1])
		if arg == "year" || arg == "год" {
			month = 0 // Бюджет на весь текущий год
		} else if strings.Contains(arg, "-") {
			// Формат YYYY-MM
			var y, m int
			if _, parseErr := fmt.Sscanf(arg, "%d-%d", &y, &m); parseErr == nil && m >= 1 && m <= 12 {
				year = y
				month = m
			}
		} else if y, parseErr := strconv.Atoi(arg); parseErr == nil && y > 2000 {
			year = y
			month = 0 // Задан конкретный год
		}
	}

	budget, err := svc.BudgetForUser(user.ID, year, month)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📊 Плановый бюджет на %s\n", budget.Period)
	b.WriteString("────────────────────────\n\n")

	if budget.TotalCount == 0 {
		b.WriteString("📭 На этот период плановых расходов не запланировано.")
		return b.String(), nil
	}

	b.WriteString("💰 Запланировано к оплате:\n")
	for _, curr := range domain.Currencies {
		if val, ok := budget.ByCurrency[curr]; ok && val > 0 {
			fmt.Fprintf(&b, "• %s: %.2f\n", curr, val)
		}
	}

	fmt.Fprintf(&b, "\n📌 Всего платежей в периоде: %d", budget.TotalCount)
	b.WriteString("\n\n💡 Совет: Для бюджета на год используйте /budget year")

	return b.String(), nil
}

// handleSetDateCommand обрабатывает команду /setdate ID ГГГГ-ММ-ДД
func handleSetDateCommand(svc *app.App, chatID string, text string) (string, error) {
	// Проверяем, что чат привязан к учетной записи
	if _, err := svc.UserByChat(chatID); err != nil {
		return "⚠️ Ваш Telegram чат не привязан к учетной записи.", nil
	}

	parts := strings.Fields(text)
	if len(parts) < 3 {
		return "⚠️ Неверный формат. Используйте команду так:\n<code>/setdate ID 2026-10-24</code>\n(ID периода можно узнать через /payments)", nil
	}

	occurrenceID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "⚠️ Некорректный ID периода.", nil
	}

	newDateStr := parts[2]

	// Проверяем существование периода
	occurrence, err := svc.Occurrence(occurrenceID)
	if err != nil {
		return "⚠️ Период с таким ID не найден.", nil
	}

	// Устанавливаем новую фактическую дату оплаты (это автоматически убирает ошибочную просрочку)[cite: 5]
	_, err = svc.SetPaid(occurrenceID, newDateStr)
	if err != nil {
		return fmt.Sprintf("⚠️ Ошибка установки даты: %v", err), nil
	}

	return fmt.Sprintf("✅ Дата оплаты для платежа %s успешно изменена на %s. Ошибочная просрочка пересчитана.",
		occurrence.PaymentName, newDateStr), nil
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
