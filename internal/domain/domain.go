// Package domain — модель предметной области Invoice Keeper: сущности, их
// инварианты и календарная арифметика.
//
// Пакет не зависит от HTTP, базы данных и Telegram.
//
// Статус периода (planned / due / overdue / paid) не хранится, а выводится из
// календарных дат: исправление даты пересчитывает просрочку само, без фоновой
// синхронизации и риска рассогласования.
package domain

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Валюты кейса: конвертации нет, поэтому суммы по разным валютам только
// группируются, но не складываются.
type Currency string

const (
	RUB Currency = "RUB"
	USD Currency = "USD"
	EUR Currency = "EUR"
)

// Currencies — допустимый набор; задаёт и порядок вывода в отчётах.
var Currencies = []Currency{RUB, USD, EUR}

func (c Currency) Valid() bool {
	for _, v := range Currencies {
		if v == c {
			return true
		}
	}
	return false
}

func (c Currency) Symbol() string {
	switch c {
	case RUB:
		return "₽"
	case USD:
		return "$"
	case EUR:
		return "€"
	default:
		return string(c)
	}
}

// Period — частота повторения. Ежемесячный повтор обязателен по условию
// кейса, остальные добавлены как расширение.
type Period string

const (
	Monthly   Period = "monthly"
	Weekly    Period = "weekly"
	Quarterly Period = "quarterly"
	HalfYear  Period = "half-year"
	Yearly    Period = "yearly"
)

var Periods = []Period{Monthly, Weekly, Quarterly, HalfYear, Yearly}

func (p Period) Valid() bool {
	for _, v := range Periods {
		if v == p {
			return true
		}
	}
	return false
}

func (p Period) months() int {
	switch p {
	case Monthly:
		return 1
	case Quarterly:
		return 3
	case HalfYear:
		return 6
	default:
		return 0
	}
}

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// Status выводится из дат, а не хранится.
type Status string

const (
	StatusPlanned Status = "planned" // срок ещё не наступил
	StatusDue     Status = "due"     // срок сегодня: оплата в день срока — вовремя
	StatusOverdue Status = "overdue" // со следующего дня после срока
	StatusPaid    Status = "paid"    // оплата подтверждена
)

// Amount хранится в минорных единицах (копейки/центы): float не участвует в
// накоплении итогов, поэтому суммы по валютам всегда точны.
type Amount int64

func ParseAmount(major float64) Amount {
	return Amount(major*100 + 0.5)
}

func (a Amount) Major() float64 { return float64(a) / 100 }

func (a Amount) String(c Currency) string {
	return fmt.Sprintf("%s %.2f", c.Symbol(), a.Major())
}

// MarshalJSON отдаёт сумму в основных единицах — так API и панель показывают
// 45000.00, а не 4500000 копеек.
func (a Amount) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(a.Major(), 'f', 2, 64)), nil
}

// User — учётная запись. Администратор заводится из .env при старте и ролью
// admin помечен, чтобы через панель его нельзя было изменить или удалить.
//
// Обычному пользователю пароль не нужен: у него нет доступа к панели, весь
// контакт с ним идёт через Telegram по TGChatID, который закрепляется за
// учётной записью после /start в боте.
type User struct {
	ID           int64     `json:"id"`
	Login        string    `json:"login"`
	PasswordHash string    `json:"-"`
	Role         Role      `json:"role"`
	TGChatID     string    `json:"tg_chat_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

// DeliveryTarget — чат, которому доставлять уведомления: персональный чат
// пользователя либо чат администратора, которому интересно всё сразу.
type DeliveryTarget struct {
	UserID int64
	ChatID string
	Admin  bool
}

// Day — календарная дата без времени суток и без часового пояса.
//
// Срок оплаты и фактическая дата — это даты, а не моменты времени. Хранить их
// как time.Time опасно: значение, разобранное из "2026-11-01", получает UTC, а
// «сегодня» приходит в поясе системы, и сравнение таких величин сдвигает
// границу суток. Поэтому Day хранит только год, месяц и число, а пояс
// участвует лишь в момент получения текущей даты.
type Day struct {
	year  int
	month time.Month
	day   int
}

// DayLayout — единственный формат дат в API, .env-примерах и БД.
const DayLayout = "2006-01-02"

// NewDay — календарная дата момента t в поясе loc.
func NewDay(t time.Time, loc *time.Location) Day {
	y, m, d := t.In(loc).Date()
	return Day{year: y, month: m, day: d}
}

// ParseDay разбирает дату формата YYYY-MM-DD.
func ParseDay(s string) (Day, error) {
	t, err := time.Parse(DayLayout, s)
	if err != nil {
		return Day{}, err
	}
	return Day{year: t.Year(), month: t.Month(), day: t.Day()}, nil
}

// Today — текущая календарная дата: все сравнения статусов идут по суткам,
// поэтому время суток на просрочку не влияет.
func Today(loc *time.Location) Day { return NewDay(time.Now(), loc) }

func (d Day) IsZero() bool   { return d == Day{} }
func (d Day) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.year, d.month, d.day) }

// Before/After/Equal сравнивают строки YYYY-MM-DD: для таких дат лексикогра-
// фический порядок совпадает с хронологическим.
func (d Day) Before(o Day) bool { return d.String() < o.String() }
func (d Day) After(o Day) bool  { return d.String() > o.String() }
func (d Day) Equal(o Day) bool  { return d == o }

func (d Day) AddDays(n int) Day {
	return NewDay(d.civil().AddDate(0, 0, n), time.UTC)
}

// AddMonths сдвигает дату на n месяцев. Если такого числа в целевом месяце
// нет, берётся последний день месяца: 31 января + 1 месяц → 28/29 февраля.
//
// time.Date здесь не подходит: он нормализует 31 февраля в 3 марта, а по
// условию кейса нужен именно последний день месяца.
func (d Day) AddMonths(n int) Day {
	total := int(d.month) - 1 + n
	year, month := d.year+total/12, time.Month(total%12+1)
	day := d.day
	if last := daysIn(year, month); day > last {
		day = last
	}
	return Day{year: year, month: month, day: day}
}

// DaysUntil — сколько суток от d до o; отрицательное, если o раньше.
func (d Day) DaysUntil(o Day) int {
	return int(o.civil().Sub(d.civil()).Hours() / 24)
}

// civil — дата как момент в UTC. UTC выбран как нейтральная зона без перехода
// на летнее время: арифметика суток и месяцев от этого не зависит.
func (d Day) civil() time.Time {
	return time.Date(d.year, d.month, d.day, 0, 0, 0, 0, time.UTC)
}

func daysIn(year int, month time.Month) int {
	// Нулевой день следующего месяца — последний день текущего.
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func (d Day) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.String() + `"`), nil
}

func (d *Day) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	parsed, err := ParseDay(s)
	if err != nil {
		return fmt.Errorf("date must be YYYY-MM-DD, got %q", s)
	}
	*d = parsed
	return nil
}

// RecurringPayment — шаблон регулярного расхода.
// UserID (0 = не назначен) связывает платёж с ответственным: все периоды
// шаблона наследуют его, поэтому привязка делается один раз на шаблоне.
type RecurringPayment struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Amount    Amount    `json:"amount"`
	Currency  Currency  `json:"currency"`
	Period    Period    `json:"period"`
	FirstDue  Day       `json:"first_due_date"`
	Active    bool      `json:"active"`
	UserID    int64     `json:"user_id,omitempty"`
	UserName  string    `json:"user_name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// DueDate возвращает календарный срок n-го периода (n = 0, 1, 2 …).
//
// День месяца является якорем от первой даты: для платежа, начавшегося
// 31 января, февральский срок — последний день февраля, а мартовский снова
// 31-е. Без якоря дата «уезжала» бы на 28-е навсегда.
func (rp *RecurringPayment) DueDate(n int) Day {
	switch {
	case rp.Period == Weekly:
		return rp.FirstDue.AddDays(7 * n)
	case rp.Period == Yearly:
		return rp.FirstDue.AddMonths(12 * n)
	default:
		return rp.FirstDue.AddMonths(rp.Period.months() * n)
	}
}

// Occurrence — конкретный период регулярного платежа: своя дата и своя
// отметка об оплате. Новый период не стирает прошлые оплаты, а просрочка не
// останавливает следующие периоды.
type Occurrence struct {
	ID           int64    `json:"id"`
	PaymentID    int64    `json:"payment_id"`
	PaymentName  string   `json:"payment_name"`
	Amount       Amount   `json:"amount"`
	Currency     Currency `json:"currency"`
	Period       Period   `json:"period"`
	DueDate      Day      `json:"due_date"`
	PaidAt       *Day     `json:"paid_at"`
	ReminderSent bool     `json:"reminder_sent"`

	// Active — признак живого шаблона: снятый платёж больше не напоминает.
	Active bool `json:"-"`

	// UserID (0 = не назначен) — ответственный за платёж, унаследованный от шаблона.
	UserID   int64  `json:"user_id,omitempty"`
	UserName string `json:"user_name,omitempty"`

	Status Status `json:"status"`
}

// ResolveStatus пересчитывает статус по календарным датам.
func (o *Occurrence) ResolveStatus(today Day) {
	switch {
	case o.PaidAt != nil:
		o.Status = StatusPaid
	case today.After(o.DueDate):
		o.Status = StatusOverdue
	case today.Equal(o.DueDate):
		o.Status = StatusDue
	default:
		o.Status = StatusPlanned
	}
}

func (o *Occurrence) Unpaid() bool { return o.PaidAt == nil }

// NewID — случайный идентификатор (jti сессии). Живёт в домене, чтобы
// прикладной слой и слой сессий не дублировали генератор.
func NewID() string { return uuid.NewString() }

// Errors — доменные ошибки для маппинга в HTTP-коды.
var (
	ErrNotFound       = errors.New("not found")
	ErrDuplicateLogin = errors.New("login already taken")
	ErrInvalidInput   = errors.New("invalid input")
)

func Invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

func (rp *RecurringPayment) Validate() error {
	switch {
	case rp.Name == "":
		return Invalid("name is required")
	case rp.Amount <= 0:
		return Invalid("amount must be greater than zero")
	case !rp.Currency.Valid():
		return Invalid("currency must be one of %v", Currencies)
	case !rp.Period.Valid():
		return Invalid("period must be one of %v", Periods)
	case rp.FirstDue.IsZero():
		return Invalid("first_due_date is required")
	}
	return nil
}
