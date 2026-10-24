// Package app — прикладной слой Invoice Keeper.
//
// Правила предметной области живут в domain, здесь — сценарии: часовой пояс,
// генерация периодов, пароли, сессии, итоги и отбор периодов для напоминаний.
package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/storage"
)

// ReminderLeadDays — за сколько дней до срока приходит напоминание.
const ReminderLeadDays = 3

// occurrenceHorizon — на сколько месяцев вперёд поддерживаются периоды.
const occurrenceHorizon = 12

// NoUserFilter отключает фильтр по ответственному. 0 при этом означает
// «платежи без ответственного», поэтому «нет фильтра» невозможно выразить
// нулём и требует отдельного значения.
const NoUserFilter int64 = -1

// App — прикладной сервис: сценарии поверх домена и хранилища.
type App struct {
	store *storage.Store
	loc   *time.Location
}

// New собирает сервис из хранилища и имени часового пояса из .env.
func New(store *storage.Store, timezone string) (*App, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("load location %q: %w", timezone, err)
	}
	return &App{store: store, loc: loc}, nil
}

// Today — текущая календарная дата в поясе из .env.
func (a *App) Today() domain.Day { return domain.Today(a.loc) }

// Store — хранилище для инфраструктурных задач: демо-данные, очистка сессий.
func (a *App) Store() *storage.Store { return a.store }

// ── инициализация ─────────────────────────────────────────────────────────

// Bootstrap заводит администратора из .env и доводит периоды до актуальных.
//
// Администратор — единственная учётная запись, которая управляется не через
// панель, а через окружение: при смене ADMIN_LOGIN старая админская запись
// переводится в обычные пользователи, чтобы в системе не осталось двух
// администраторов и вход не открывался под прежним логином.
func (a *App) Bootstrap(login, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}

	u, err := a.store.UserByLogin(login)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		if _, err := a.store.CreateUser(login, string(hash), string(domain.RoleAdmin)); err != nil {
			return fmt.Errorf("create admin: %w", err)
		}
	case err != nil:
		return err
	case !u.IsAdmin():
		// Логин из .env занят обычным пользователем — администратор не может
		// его перехватить, иначе пользователь получил бы права админа.
		return fmt.Errorf("login %q is already used by a non-admin user", login)
	default:
		// Пароль в .env — источник истины: админ не меняется через панель,
		// значит при рестарте его пароль должен совпадать с окружением.
		if err := a.store.UpdateUser(u.ID, "", string(hash)); err != nil {
			return fmt.Errorf("sync admin password: %w", err)
		}
	}

	if err := a.store.DemoteAdminsExcept(login); err != nil {
		return fmt.Errorf("demote stale admins: %w", err)
	}
	return a.Sync()
}

// Sync доводит наборы периодов всех активных платежей до актуальных.
// Запуск после рестарта гарантирует, что пропущенные за время простоя периоды
// появятся в списке.
func (a *App) Sync() error {
	payments, err := a.store.ListPayments(true)
	if err != nil {
		return err
	}
	today := a.Today()
	for i := range payments {
		if err := a.store.SyncOccurrences(&payments[i], today, occurrenceHorizon); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) DeletePayment(id int64) error {
	return a.store.DeletePayment(id)
}

// ── доступ ────────────────────────────────────────────────────────────────

// Authenticate проверяет учетные данные пользователя или администратора
func (a *App) Authenticate(login, password string) (*domain.User, error) {
	if strings.TrimSpace(login) == "" || strings.TrimSpace(password) == "" {
		return nil, fmt.Errorf("укажите логин и пароль")
	}

	// Ищем пользователя по логину в базе данных
	user, err := a.store.UserByLogin(login)
	if err != nil {
		return nil, fmt.Errorf("неверный логин или пароль")
	}

	// Сверяем пароль через bcrypt с хэшем в базе
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, fmt.Errorf("неверный логин или пароль")
	}

	return user, nil
}

// RevokeSession вносит сессию в чёрный список: logout делает выданный токен
// непригодным до его естественного истечения.
func (a *App) RevokeSession(jti string, expiresAt time.Time) error {
	return a.store.RevokeToken(jti, expiresAt)
}

// SessionRevoked — проверка токена на отзыв при каждом запросе.
func (a *App) SessionRevoked(jti string) (bool, error) {
	return a.store.IsTokenRevoked(jti)
}

// ── пользователи ──────────────────────────────────────────────────────────
// unusableHash — заведомо неверный хеш для обычных пользователей, у которых нет доступа к панели.
const unusableHash = "$2a$10$UnusableHashForRegularUsersWithoutPanelAccess000000000000000"

// CreateUser создаёт обычного пользователя. Пароль не задается: у обычной
// учётной записи нет доступа к панели, весь её контакт с системой — Telegram
// по закреплённому чату. Поэтому в базе остаётся заведомо неверный хеш.
func (a *App) CreateUser(login string) (*domain.User, error) {
	if len(login) < 3 {
		return nil, domain.Invalid("login must be at least 3 characters")
	}
	id, err := a.store.CreateUser(login, unusableHash, string(domain.RoleUser))
	if err != nil {
		return nil, err
	}
	return a.store.UserByID(id)
}

// CreateUserWithPassword — корректное создание пользователя с хэшированием пароля
func (a *App) CreateUserWithPassword(login, password string) (*domain.User, error) {
	if strings.TrimSpace(login) == "" {
		return nil, domain.Invalid("логин не может быть пустым")
	}
	if len(password) < 4 {
		return nil, domain.Invalid("пароль должен быть не менее 4 символов")
	}

	// Обязательно генерируем bcrypt-хэш для пароля обычного пользователя!
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	// Сохраняем пользователя с реальным хэшем в базе
	userID, err := a.store.CreateUser(login, string(hash), string(domain.RoleUser))
	if err != nil {
		return nil, err
	}

	return a.store.UserByID(userID)
}

func (a *App) Users() ([]domain.User, error) { return a.store.ListUsers() }

func (a *App) User(id int64) (*domain.User, error) { return a.store.UserByID(id) }

// UserByChat — учётная запись по Telegram-чату.
func (a *App) UserByChat(chatID string) (*domain.User, error) {
	if chatID == "" {
		return nil, domain.ErrNotFound
	}
	return a.store.UserByChat(chatID)
}

// UpdateUser меняет логин; пустое значение означает «не менять». Учётная
// запись администратора защищена: она управляется только через .env.
func (a *App) UpdateUser(id int64, login string) (*domain.User, error) {
	target, err := a.store.UserByID(id)
	if err != nil {
		return nil, err
	}
	if target.IsAdmin() {
		return nil, domain.Invalid("учётная запись администратора управляется через .env")
	}
	if login == "" || login == target.Login {
		return target, nil
	}
	if len(login) < 3 {
		return nil, domain.Invalid("login must be at least 3 characters")
	}
	if err := a.store.UpdateUser(id, login, ""); err != nil {
		return nil, err
	}
	return a.store.UserByID(id)
}

// DeleteUser удаляет обычного пользователя; администратор защищён.
func (a *App) DeleteUser(id int64) error {
	target, err := a.store.UserByID(id)
	if err != nil {
		return err
	}
	if target.IsAdmin() {
		return domain.Invalid("учётная запись администратора управляется через .env")
	}
	return a.store.DeleteUser(id)
}

// validateCredentials — общие правила логина и пароля для создания и смены.
func validateCredentials(login, password string) error {
	if login != "" && len(login) < 3 {
		return domain.Invalid("login must be at least 3 characters")
	}
	if len(password) < 6 {
		return domain.Invalid("password must be at least 6 characters")
	}
	return nil
}

type AuditLog struct {
	ID        int64
	UserID    *int64
	UserName  string
	Type      string
	TypeLabel string
	Action    string
	IPSource  string
	CreatedAt time.Time
}

func (a *App) LogEvent(userID *int64, userName, eventType, typeLabel, action, ipSource string) {
	_ = a.store.CreateAuditLog(userID, userName, eventType, typeLabel, action, ipSource)
}

func (a *App) AuditLogs() ([]AuditLog, error) {
	records, err := a.store.ListAuditLogs()
	if err != nil {
		return nil, err
	}
	var out []AuditLog
	for _, r := range records {
		out = append(out, AuditLog{
			ID:        r.ID,
			UserID:    r.UserID,
			UserName:  r.UserName,
			Type:      r.Type,
			TypeLabel: r.TypeLabel,
			Action:    r.Action,
			IPSource:  r.IPSource,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// ── регулярные платежи ────────────────────────────────────────────────────

// PaymentInput — данные для создания и изменения шаблона. Нулевые поля
// означают «не менять» — поэтому тип общий для POST и PATCH.
type PaymentInput struct {
	Name     string          `json:"name"`
	Amount   float64         `json:"amount"`
	Currency domain.Currency `json:"currency"`
	Period   domain.Period   `json:"period"`
	DueDate  string          `json:"due_date"`
	UserID   int64           `json:"user_id"`
}
type BudgetTotals struct {
	Period     string // например: "2026-10" или "2026 год"
	ByCurrency map[domain.Currency]float64
	TotalCount int
}
type CalendarDay struct {
	Date  string
	Items []domain.Occurrence
}

func (a *App) MonthlyCalendar(userID int64, year int, month int) ([]CalendarDay, string, error) {
	occurrences, err := a.OccurrencesFiltered(0, userID)
	if err != nil {
		return nil, "", err
	}

	periodStr := fmt.Sprintf("%04d-%02d", year, month)
	daysMap := make(map[string][]domain.Occurrence)

	for _, o := range occurrences {
		dueStr := o.DueDate.String()
		if !strings.HasPrefix(dueStr, periodStr) {
			continue
		}
		daysMap[dueStr] = append(daysMap[dueStr], o)
	}

	// Сортируем дни по возрастанию дат
	var dates []string
	for d := range daysMap {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	var calendar []CalendarDay
	for _, d := range dates {
		calendar = append(calendar, CalendarDay{
			Date:  d,
			Items: daysMap[d],
		})
	}

	return calendar, periodStr, nil
}

// BudgetForUser рассчитывает плановые расходы пользователя за указанный год и месяц (если month > 0).
// Если month == 0, рассчитывается плановый бюджет на весь год.
func (a *App) BudgetForUser(userID int64, year int, month int) (*BudgetTotals, error) {
	// Получаем все периоды пользователя
	occurrences, err := a.OccurrencesFiltered(0, userID)
	if err != nil {
		return nil, err
	}

	totals := &BudgetTotals{
		ByCurrency: make(map[domain.Currency]float64),
	}

	var prefix string
	if month > 0 {
		prefix = fmt.Sprintf("%04d-%02d", year, month)
		totals.Period = prefix
	} else {
		prefix = fmt.Sprintf("%04d", year)
		totals.Period = fmt.Sprintf("%04d год", year)
	}

	for _, o := range occurrences {
		// Фильтруем по строковому префиксу даты (например, "2026-10" или "2026")
		if !strings.HasPrefix(o.DueDate.String(), prefix) {
			continue
		}

		totals.ByCurrency[o.Currency] += o.Amount.Major()
		totals.TotalCount++
	}

	return totals, nil
}

// CreatePayment сохраняет шаблон и создаёт его периоды.
func (a *App) CreatePayment(in PaymentInput) (*domain.RecurringPayment, error) {
	due, err := domain.ParseDay(in.DueDate)
	if err != nil {
		return nil, domain.Invalid("due_date must be YYYY-MM-DD, got %q", in.DueDate)
	}

	p := domain.RecurringPayment{
		Name:     in.Name,
		Amount:   domain.ParseAmount(in.Amount),
		Currency: in.Currency,
		Period:   in.Period,
		FirstDue: due,
		UserID:   in.UserID,
		Active:   true,
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := a.checkUser(p.UserID); err != nil {
		return nil, err
	}

	if err := a.store.CreatePayment(&p); err != nil {
		return nil, err
	}
	// Один покрывает и обычную генерацию, и «платёж добавлен позже, чем за
	// 3 дня до срока»: первый период создаётся даже наступившим, поэтому
	// напоминание уйдёт сразу.
	if err := a.store.SyncOccurrences(&p, a.Today(), occurrenceHorizon); err != nil {
		return nil, err
	}
	return a.Payment(p.ID)
}

func (a *App) Payment(id int64) (*domain.RecurringPayment, error) {
	return a.store.PaymentByID(id)
}

func (a *App) Payments(activeOnly bool) ([]domain.RecurringPayment, error) {
	return a.store.ListPayments(activeOnly)
}

func (a *App) SetUserChat(userID int64, chatID string) error {
	return a.store.SetUserChat(userID, chatID)
}

// PaymentsFiltered — шаблоны с фильтром по ответственному: NoUserFilter без
// фильтра, 0 — без ответственного, N — закреплённые за пользователем N.
func (a *App) PaymentsFiltered(activeOnly bool, userID int64) ([]domain.RecurringPayment, error) {
	return a.store.ListPaymentsFiltered(activeOnly, userID)
}

// UpdatePayment меняет шаблон. Смена срока или периодичности пересобирает
// будущие периоды: старые даты после такого расчёта были бы неверны.
// Оплаченные периоды остаются нетронутыми.
func (a *App) UpdatePayment(id int64, in PaymentInput) (*domain.RecurringPayment, error) {
	current, err := a.store.PaymentByID(id)
	if err != nil {
		return nil, err
	}

	updated := *current
	if in.Name != "" {
		updated.Name = in.Name
	}
	if in.Amount > 0 {
		updated.Amount = domain.ParseAmount(in.Amount)
	}
	if in.Currency != "" {
		updated.Currency = in.Currency
	}
	if in.Period != "" {
		updated.Period = in.Period
	}
	if in.DueDate != "" {
		due, err := domain.ParseDay(in.DueDate)
		if err != nil {
			return nil, domain.Invalid("due_date must be YYYY-MM-DD, got %q", in.DueDate)
		}
		updated.FirstDue = due
	}
	if err := a.checkUser(in.UserID); err != nil {
		return nil, err
	}
	if in.UserID > 0 {
		updated.UserID = in.UserID
	}

	if err := updated.Validate(); err != nil {
		return nil, err
	}

	var amount *int64
	if in.Amount > 0 {
		raw := int64(updated.Amount)
		amount = &raw
	}
	if err := a.store.UpdatePayment(id, in.Name, amount, string(in.Currency),
		string(in.Period), in.DueDate, updated.UserID); err != nil {
		return nil, err
	}

	if updated.Period != current.Period || updated.FirstDue != current.FirstDue {
		if err := a.store.DropFutureOccurrences(id, a.Today()); err != nil {
			return nil, err
		}
	}
	if updated.Active {
		if err := a.store.SyncOccurrences(&updated, a.Today(), occurrenceHorizon); err != nil {
			return nil, err
		}
	}
	return a.store.PaymentByID(id)
}

// AssignPaymentUser назначает ответственного (0 — снять привязку). Периоды
// наследуют его от шаблона, поэтому достаточно одного поля.
func (a *App) AssignPaymentUser(paymentID, userID int64) (*domain.RecurringPayment, error) {
	if err := a.checkUser(userID); err != nil {
		return nil, err
	}
	if err := a.store.SetPaymentUser(paymentID, userID); err != nil {
		return nil, err
	}
	return a.store.PaymentByID(paymentID)
}

// StopPayment прекращает платёж и его напоминания, история оплат сохраняется.
func (a *App) StopPayment(id int64) error {
	return a.store.DeactivatePayment(id)
}

// ResumePayment возобновляет остановленный платёж и догоняет пропущенные
// периоды.
func (a *App) ResumePayment(id int64) (*domain.RecurringPayment, error) {
	if err := a.store.ReactivatePayment(id); err != nil {
		return nil, err
	}
	p, err := a.store.PaymentByID(id)
	if err != nil {
		return nil, err
	}
	if err := a.store.SyncOccurrences(p, a.Today(), occurrenceHorizon); err != nil {
		return nil, err
	}
	return p, nil
}

// checkUser проверяет, что назначаемый ответственный существует. Ноль —
// «ответственный не назначен», это допустимо.
func (a *App) checkUser(id int64) error {
	if id == 0 {
		return nil
	}
	if _, err := a.store.UserByID(id); err != nil {
		if domain.IsNotFound(err) {
			return domain.Invalid("ответственный не найден")
		}
		return err
	}
	return nil
}

// ── периоды ────────────────────────────────────────────────────────────────

// Occurrences — периоды с пересчитанными статусами; paymentID = 0 — все платежи.
func (a *App) Occurrences(paymentID int64) ([]domain.Occurrence, error) {
	return a.occurrences(paymentID, NoUserFilter)
}

// OccurrencesFiltered — периоды по шаблону и ответственному: paymentID = 0 —
// любые, userID = NoUserFilter без фильтра, 0 — без ответственного.
func (a *App) OccurrencesFiltered(paymentID, userID int64) ([]domain.Occurrence, error) {
	return a.occurrences(paymentID, userID)
}

func (a *App) occurrencesAssigned(userID int64) ([]domain.Occurrence, error) {
	return a.occurrences(0, userID)
}

func (a *App) occurrences(paymentID, userID int64) ([]domain.Occurrence, error) {
	list, err := a.store.ListOccurrencesFiltered(paymentID, userID)
	if err != nil {
		return nil, err
	}
	today := a.Today()
	for i := range list {
		list[i].ResolveStatus(today)
	}
	return list, nil
}

// Occurrence возвращает один период.
func (a *App) Occurrence(id int64) (*domain.Occurrence, error) {
	o, err := a.store.OccurrenceByID(id)
	if err != nil {
		return nil, err
	}
	o.ResolveStatus(a.Today())
	return o, nil
}

// SetPaid выставляет отметку об оплате либо снимает её.
//
// paidAt == "" снимает отметку и очищает дату; статус пересчитывается из
// due_date, поэтому исправление даты убирает ошибочную просрочку само.
func (a *App) SetPaid(id int64, paidAt string) (*domain.Occurrence, error) {
	var target *domain.Day
	if paidAt != "" {
		d, err := domain.ParseDay(paidAt)
		if err != nil {
			return nil, domain.Invalid("paid_at must be YYYY-MM-DD, got %q", paidAt)
		}
		target = &d
	}
	if err := a.store.SetPaid(id, target); err != nil {
		return nil, err
	}
	return a.Occurrence(id)
}

// SetOccurrenceDue переносит срок отдельного периода, не трогая шаблон:
// разовые сдвиги графика не должны менять будущие периоды.
func (a *App) SetOccurrenceDue(id int64, dueDate string) (*domain.Occurrence, error) {
	due, err := domain.ParseDay(dueDate)
	if err != nil {
		return nil, domain.Invalid("due_date must be YYYY-MM-DD, got %q", dueDate)
	}
	if err := a.store.SetOccurrenceDate(id, due); err != nil {
		return nil, err
	}
	return a.Occurrence(id)
}

// ── итоги ──────────────────────────────────────────────────────────────────

// CurrencyTotals — сумма по одной валюте. Отдельно на каждую валюту:
// конвертация по условию кейса не выполняется.
type CurrencyTotals struct {
	Currency    domain.Currency `json:"currency"`
	Planned     float64         `json:"planned"`
	Due         float64         `json:"due"`
	Overdue     float64         `json:"overdue"`
	Paid        float64         `json:"paid"`
	Outstanding float64         `json:"outstanding"`
	Total       float64         `json:"total"`
	Count       int             `json:"count"`
}

type StatusTotals struct {
	Planned int `json:"planned"`
	Due     int `json:"due"`
	Overdue int `json:"overdue"`
	Paid    int `json:"paid"`
	Total   int `json:"total"`
}

// Totals — сводка на дату. Итоги по валютам не складываются между собой.
type Totals struct {
	Date       string           `json:"date"`
	Status     StatusTotals     `json:"status"`
	ByCurrency []CurrencyTotals `json:"by_currency"`
}

// Summary собирает итоги по всем периодам на текущую дату.
func (a *App) Summary() (*Totals, error) { return a.summaryOf(0, false) }

// SummaryAssigned собирает итоги по периодам, закреплённым за пользователем
// (0 — без ответственного).
func (a *App) SummaryAssigned(userID int64) (*Totals, error) { return a.summaryOf(userID, true) }

func (a *App) summaryOf(userID int64, assigned bool) (*Totals, error) {
	var (
		list []domain.Occurrence
		err  error
	)
	if assigned {
		list, err = a.occurrencesAssigned(userID)
	} else {
		list, err = a.Occurrences(0)
	}
	if err != nil {
		return nil, err
	}

	totals := &Totals{Date: a.Today().String()}
	acc := map[domain.Currency]*CurrencyTotals{}
	for _, c := range domain.Currencies {
		acc[c] = &CurrencyTotals{Currency: c}
	}

	for _, o := range list {
		totals.Status.Total++

		c := acc[o.Currency]
		c.Count++
		c.Total += o.Amount.Major()

		switch o.Status {
		case domain.StatusPaid:
			totals.Status.Paid++
			c.Paid += o.Amount.Major()
		case domain.StatusPlanned:
			totals.Status.Planned++
			c.Planned += o.Amount.Major()
			c.Outstanding += o.Amount.Major()
		case domain.StatusDue:
			totals.Status.Due++
			c.Due += o.Amount.Major()
			c.Outstanding += o.Amount.Major()
		case domain.StatusOverdue:
			totals.Status.Overdue++
			c.Overdue += o.Amount.Major()
			c.Outstanding += o.Amount.Major()
		}
	}

	// Порядок валют фиксирован доменом: RUB, USD, EUR.
	totals.ByCurrency = make([]CurrencyTotals, 0, len(acc))
	for _, c := range domain.Currencies {
		if acc[c].Count > 0 {
			totals.ByCurrency = append(totals.ByCurrency, *acc[c])
		}
	}
	return totals, nil
}

func (a *App) SummaryOnDate(targetDate string) (*Totals, error) {
	return a.summaryOfDate(0, false, targetDate)
}

// SummaryAssignedOnDate собирает итоги для конкретного пользователя на указанную дату.
func (a *App) SummaryAssignedOnDate(userID int64, targetDate string) (*Totals, error) {
	return a.summaryOfDate(userID, true, targetDate)
}

func (a *App) summaryOfDate(userID int64, assigned bool, targetDate string) (*Totals, error) {
	var (
		list []domain.Occurrence
		err  error
	)
	if assigned {
		list, err = a.occurrencesAssigned(userID)
	} else {
		list, err = a.Occurrences(0)
	}
	if err != nil {
		return nil, err
	}

	// Если дата не указана, используем сегодня
	if targetDate == "" {
		targetDate = a.Today().String()
	}

	totals := &Totals{Date: targetDate}
	acc := map[domain.Currency]*CurrencyTotals{}
	for _, c := range domain.Currencies {
		acc[c] = &CurrencyTotals{Currency: c}
	}

	for _, o := range list {
		// Учитываем только платежи, чей срок равен или предшествует указанной дате
		if o.DueDate.String() > targetDate {
			continue
		}

		totals.Status.Total++
		c := acc[o.Currency]
		c.Count++
		c.Total += o.Amount.Major()

		switch o.Status {
		case domain.StatusPaid:
			totals.Status.Paid++
			c.Paid += o.Amount.Major()
		case domain.StatusPlanned:
			totals.Status.Planned++
			c.Planned += o.Amount.Major()
			c.Outstanding += o.Amount.Major()
		case domain.StatusDue:
			totals.Status.Due++
			c.Due += o.Amount.Major()
			c.Outstanding += o.Amount.Major()
		case domain.StatusOverdue:
			totals.Status.Overdue++
			c.Overdue += o.Amount.Major()
			c.Outstanding += o.Amount.Major()
		}
	}

	totals.ByCurrency = make([]CurrencyTotals, 0, len(acc))
	for _, c := range domain.Currencies {
		if acc[c].Count > 0 {
			totals.ByCurrency = append(totals.ByCurrency, *acc[c])
		}
	}
	return totals, nil
}

// ── Telegram-подписчики ───────────────────────────────────────────────────

// SubscribeChat включает напоминания для чата, /unsubscribe — выключает.
func (a *App) SubscribeChat(chatID string) error { return a.store.AddTelegramChat(chatID) }

// UnsubscribeChat убирает чат из списка получателей.
func (a *App) UnsubscribeChat(chatID string) error { return a.store.RemoveTelegramChat(chatID) }

// SubscribedChats — все чаты, которым отправлять напоминания.
func (a *App) SubscribedChats() ([]string, error) { return a.store.ListTelegramChats() }

func (a *App) Subscribed(chatID string) (bool, error) {
	chats, err := a.store.ListTelegramChats()
	if err != nil {
		return false, err
	}
	for _, c := range chats {
		if c == chatID {
			return true, nil
		}
	}
	return false, nil
}

// HasSubscribers — подписан ли хоть один чат. Нужно для автоподписки чата из
// .env: конфиг остаётся рабочим, но не мешает подписке через /start.
func (a *App) HasSubscribers() (bool, error) { return a.store.HasTelegramChats() }

// ── напоминания ───────────────────────────────────────────────────────────

// PendingReminders — периоды, требующие напоминания.
//
// Отбираются неоплаченные периоды активных платежей со сроком не позднее чем
// через ReminderLeadDays дней. Это покрывает и плановое напоминание за 3 дня,
// и требование «платёж добавлен позже чем за 3 дня до срока → напоминание
// сразу». Дубли исключает флаг reminder_sent, а снятый платёж выпадает из
// выборки.
func (a *App) PendingReminders() ([]domain.Occurrence, error) {
	limit := a.Today().AddDays(ReminderLeadDays)

	list, err := a.store.ListOccurrences(0)
	if err != nil {
		return nil, err
	}

	out := make([]domain.Occurrence, 0, len(list))
	today := a.Today()
	for _, o := range list {
		o.ResolveStatus(today)
		switch {
		case !o.Active:
			continue // снятый платёж больше не напоминает
		case !o.Unpaid():
			continue // оплаченный период не напоминает
		case o.ReminderSent:
			continue // дубль
		case o.DueDate.After(limit):
			continue // слишком рано
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DueDate.Before(out[j].DueDate) })
	return out, nil
}

// PendingRemindersForUser возвращает напоминания строго для указанного пользователя.
func (a *App) PendingRemindersForUser(userID int64) ([]domain.Occurrence, error) {
	limit := a.Today().AddDays(ReminderLeadDays)

	// Получаем периоды, отфильтрованные по ID пользователя
	list, err := a.store.ListOccurrencesFiltered(0, userID)
	if err != nil {
		return nil, err
	}

	out := make([]domain.Occurrence, 0, len(list))
	today := a.Today()
	for _, o := range list {
		o.ResolveStatus(today)
		switch {
		case !o.Active:
			continue
		case !o.Unpaid():
			continue
		case o.ReminderSent:
			continue
		case o.DueDate.After(limit):
			continue
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DueDate.Before(out[j].DueDate) })
	return out, nil
}

// ClaimReminder атомарно занимает напоминание: true вернётся ровно один раз
// для каждого периода, даже при параллельных запусках планировщика.
func (a *App) ClaimReminder(id int64) (bool, error) {
	return a.store.MarkReminderSent(id)
}

// ReleaseReminder возвращает напоминание в очередь, если доставка не удалась.
func (a *App) ReleaseReminder(id int64) error {
	return a.store.ClearReminder(id)
}

func ReminderText(o domain.Occurrence, today domain.Day) string {
	left := today.DaysUntil(o.DueDate)
	switch {
	case left < 0:
		return fmt.Sprintf("🔴 Просрочено на %d дн.\n\n%s — %s\nСрок был: %s",
			-left, o.PaymentName, o.Amount.String(o.Currency), o.DueDate)
	case left == 0:
		return fmt.Sprintf("⏰ Оплатить сегодня\n\n%s — %s\nСрок: %s",
			o.PaymentName, o.Amount.String(o.Currency), o.DueDate)
	default:
		return fmt.Sprintf("⏰ Напоминание об оплате\n\n%s — %s\nСрок: %s (через %d дн.)",
			o.PaymentName, o.Amount.String(o.Currency), o.DueDate, left)
	}
}
