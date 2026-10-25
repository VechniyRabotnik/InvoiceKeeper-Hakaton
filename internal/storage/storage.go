// Package storage — слой персистентности на SQLite (modernc, чистый Go без cgo).
//
// Даты лежат в TEXT в формате 'YYYY-MM-DD' — ровно в той календарной модели,
// в которой их трактует домен. Статус периода не хранится: он выводится из
// due_date и paid_at и не может разойтись с датами после их исправления.
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/VechniyRabotnik/InvoiceKeeper-Hakaton/internal/domain"
)

// Store — доступ к базе. Репозиторий отдаёт доменные типы и не знает о HTTP.
type Store struct{ db *sql.DB }

// New открывает базу, включает обязательные PRAGMA и создаёт схему.
//
// Каталог базы создаётся при необходимости: data/ не хранится в репозитории,
// и без этого чистое клонирование падало бы на первом запуске.
func New(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite допускает одну пишущую горутину; WAL ускоряет параллельные чтения.
	db.SetMaxOpenConns(1)

	// foreign_keys в SQLite по умолчанию выключен: без него ON DELETE CASCADE
	// не работает и периоды удалённого платежа остаются сиротами.
	for _, pragma := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	login         TEXT    NOT NULL UNIQUE,
	password_hash TEXT    NOT NULL,
	role          TEXT    NOT NULL DEFAULT 'user',
	tg_chat_id    TEXT    UNIQUE,
	created_at    TEXT    NOT NULL
);
CREATE TABLE IF NOT EXISTS recurring_payments (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	name           TEXT    NOT NULL,
	amount         INTEGER NOT NULL,
	currency       TEXT    NOT NULL,
	period         TEXT    NOT NULL,
	first_due_date TEXT    NOT NULL,
	active         INTEGER NOT NULL DEFAULT 1,
	user_id        INTEGER REFERENCES users(id) ON DELETE SET NULL,
	created_at     TEXT    NOT NULL
);
CREATE TABLE IF NOT EXISTS occurrences (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	payment_id    INTEGER NOT NULL REFERENCES recurring_payments(id) ON DELETE CASCADE,
	due_date      TEXT    NOT NULL,
	paid_at       TEXT,
	reminder_sent INTEGER NOT NULL DEFAULT 0,
	UNIQUE (payment_id, due_date)
);
CREATE INDEX IF NOT EXISTS idx_occ_due ON occurrences(due_date);
CREATE TABLE IF NOT EXISTS revoked_tokens (
	jti        TEXT PRIMARY KEY,
	revoked_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS telegram_chats (
	chat_id    TEXT PRIMARY KEY,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_logs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    INTEGER,
	user_name  TEXT    NOT NULL,
	type       TEXT    NOT NULL,
	type_label TEXT    NOT NULL,
	action     TEXT    NOT NULL,
	ip_source  TEXT    NOT NULL,
	created_at TEXT    NOT NULL
);
`

// ── Журнал аудита ──────────────────────────────────────────────────────────

func (s *Store) CreateAuditLog(userID *int64, userName, eventType, typeLabel, action, ipSource string) error {
	var uID any
	if userID != nil && *userID > 0 {
		uID = *userID
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_logs (user_id, user_name, type, type_label, action, ip_source, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uID, userName, eventType, typeLabel, action, ipSource, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

type AuditLogRecord struct {
	ID        int64
	UserID    *int64
	UserName  string
	Type      string
	TypeLabel string
	Action    string
	IPSource  string
	CreatedAt time.Time
}

func (s *Store) ListAuditLogs() ([]AuditLogRecord, error) {
	rows, err := s.db.Query(`SELECT id, user_id, user_name, type, type_label, action, ip_source, created_at FROM audit_logs ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditLogRecord
	for rows.Next() {
		var (
			l       AuditLogRecord
			uID     sql.NullInt64
			created string
		)
		if err := rows.Scan(&l.ID, &uID, &l.UserName, &l.Type, &l.TypeLabel, &l.Action, &l.IPSource, &created); err != nil {
			return nil, err
		}
		if uID.Valid {
			idVal := uID.Int64
			l.UserID = &idVal
		}
		l.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, l)
	}
	return out, rows.Err()
}

// migrate создаёт схему и доводит до неё старые базы: колонки user_id и
// tg_chat_id появились позже своих таблиц, поэтому их наличие проверяется явно.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	changes := []struct{ table, column, ddl string }{
		{"recurring_payments", "user_id",
			`ALTER TABLE recurring_payments ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE SET NULL`},
		{"users", "tg_chat_id", `ALTER TABLE users ADD COLUMN tg_chat_id TEXT`},
	}
	for _, c := range changes {
		if err := addColumnIfMissing(s.db, c.table, c.column, c.ddl); err != nil {
			return err
		}
	}

	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_tg_chat_id ON users(tg_chat_id) WHERE tg_chat_id IS NOT NULL`); err != nil {
		return err
	}

	return nil
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	return n > 0, err
}

func addColumnIfMissing(db *sql.DB, table, column, ddl string) error {
	ok, err := columnExists(db, table, column)
	if err != nil || ok {
		return err
	}
	_, err = db.Exec(ddl)
	return err
}

// ── пользователи ──────────────────────────────────────────────────────────

func (s *Store) CreateUser(login, hash, role string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO users (login, password_hash, role, created_at) VALUES (?,?,?,?)`,
		login, hash, role, now())
	if err != nil {
		if uniqueViolation(err) {
			return 0, domain.ErrDuplicateLogin
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UserByLogin(login string) (*domain.User, error) {
	return s.user(`login = ?`, login)
}

func (s *Store) UserByID(id int64) (*domain.User, error) {
	return s.user(`id = ?`, id)
}

// UserByChat — учётная запись, к которой привязан чат. Нужна боту, чтобы по
// нажатию кнопки и при напоминании понимать, кто отвечает за платёж.
func (s *Store) UserByChat(chatID string) (*domain.User, error) {
	return s.user(`tg_chat_id = ?`, chatID)
}

func (s *Store) user(where string, arg any) (*domain.User, error) {
	var (
		u       domain.User
		role    string
		chat    sql.NullString
		created string
	)
	err := s.db.QueryRow(
		`SELECT id, login, password_hash, role, tg_chat_id, created_at FROM users WHERE `+where, arg,
	).Scan(&u.ID, &u.Login, &u.PasswordHash, &role, &chat, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Role = domain.Role(role)
	u.TGChatID = chat.String
	u.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &u, nil
}

func (s *Store) ListUsers() ([]domain.User, error) {
	rows, err := s.db.Query(`SELECT id, login, role, tg_chat_id, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.User
	for rows.Next() {
		var (
			u       domain.User
			role    string
			chat    sql.NullString
			created string
		)
		if err := rows.Scan(&u.ID, &u.Login, &role, &chat, &created); err != nil {
			return nil, err
		}
		u.Role = domain.Role(role)
		u.TGChatID = chat.String
		u.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser меняет непустые поля: логин и/или хеш пароля.
func (s *Store) UpdateUser(id int64, login, hash string) error {
	res, err := s.db.Exec(`UPDATE users
		SET login         = COALESCE(NULLIF(?, ''), login),
		    password_hash = COALESCE(NULLIF(?, ''), password_hash)
		WHERE id = ?`, login, hash, id)
	if err != nil {
		if uniqueViolation(err) {
			return domain.ErrDuplicateLogin
		}
		return err
	}
	return affected(res)
}

func (s *Store) DeleteUser(id int64) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) DeletePayment(id int64) error {
	res, err := s.db.Exec(`DELETE FROM recurring_payments WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// DemoteAdminsExcept понижает до обычных пользователей все админские записи,
// кроме указанного логина. Вызывается при старте: администратор задаётся
// только через .env, поэтому «осиротевшая» админская запись после смены
// ADMIN_LOGIN не должна сохранять права.
func (s *Store) DemoteAdminsExcept(login string) error {
	_, err := s.db.Exec(`UPDATE users SET role = ? WHERE role = ? AND login <> ?`,
		string(domain.RoleUser), string(domain.RoleAdmin), login)
	return err
}

// SetUserChat закрепляет за пользователем Telegram-чат; пустая строка снимает
// привязку. Чат уникален: один чат не может обслуживать двух пользователей,
// иначе напоминания ушли бы не тому адресату.
func (s *Store) SetUserChat(id int64, chatID string) error {
	var value any
	if chatID != "" {
		value = chatID
	}
	res, err := s.db.Exec(`UPDATE users SET tg_chat_id = ? WHERE id = ?`, value, id)
	if err != nil {
		if uniqueViolation(err) {
			return domain.Invalid("чат уже привязан к другому пользователю")
		}
		return err
	}
	return affected(res)
}

// ── регулярные платежи ────────────────────────────────────────────────────

const paymentCols = `p.id, p.name, p.amount, p.currency, p.period, p.first_due_date, p.active, p.user_id, u.login, p.created_at`

const paymentFrom = `FROM recurring_payments p LEFT JOIN users u ON u.id = p.user_id`

func scanPayment(scan func(...any) error) (domain.RecurringPayment, error) {
	var (
		p            domain.RecurringPayment
		amount       int64
		active       int
		userID       sql.NullInt64
		userName     sql.NullString
		first, added string
	)
	err := scan(&p.ID, &p.Name, &amount, &p.Currency, &p.Period, &first, &active, &userID, &userName, &added)
	if err != nil {
		return p, err
	}
	p.Amount, p.Active = domain.Amount(amount), active == 1
	p.FirstDue = day(first)
	p.UserID = userID.Int64
	p.UserName = userName.String
	p.CreatedAt, _ = time.Parse(time.RFC3339, added)
	return p, nil
}

func (s *Store) CreatePayment(p *domain.RecurringPayment) error {
	created := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO recurring_payments
		 (name, amount, currency, period, first_due_date, active, user_id, created_at)
		 VALUES (?,?,?,?,?,1,?,?)`,
		p.Name, int64(p.Amount), p.Currency, p.Period, p.FirstDue.String(), nullInt64(p.UserID),
		created.Format(time.RFC3339))
	if err != nil {
		if foreignKeyViolation(err) {
			return domain.Invalid("ответственный не найден")
		}
		return err
	}
	p.ID, _ = res.LastInsertId()
	// Шаблон уходит в ответ сразу после вставки, без повторного чтения из БД.
	p.CreatedAt = created
	p.Active = true
	return nil
}

// foreignKeyViolation срабатывает, когда ответственного удалили между
// проверкой и записью: нарушается ссылка recurring_payments.user_id.
func foreignKeyViolation(err error) bool {
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

func (s *Store) PaymentByID(id int64) (*domain.RecurringPayment, error) {
	p, err := scanPayment(s.db.QueryRow(
		`SELECT `+paymentCols+` `+paymentFrom+` WHERE p.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) ListPayments(activeOnly bool) ([]domain.RecurringPayment, error) {
	return s.ListPaymentsFiltered(activeOnly, -1)
}

// ListPaymentsFiltered отдаёт шаблоны, при необходимости — только указанного
// ответственного (userID = -1 без фильтра, 0 — без ответственного).
func (s *Store) ListPaymentsFiltered(activeOnly bool, userID int64) ([]domain.RecurringPayment, error) {
	var where []string
	args := []any{}
	if activeOnly {
		where = append(where, `p.active = 1`)
	}
	switch {
	case userID > 0:
		where = append(where, `p.user_id = ?`)
		args = append(args, userID)
	case userID == 0:
		where = append(where, `p.user_id IS NULL`)
	}

	q := `SELECT ` + paymentCols + ` ` + paymentFrom
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	rows, err := s.db.Query(q+` ORDER BY p.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.RecurringPayment
	for rows.Next() {
		p, err := scanPayment(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePayment меняет непустые поля шаблона и ответственного; newPeriod > 0
// означает «периодичность поменялась» и вызывает пересчёт будущих периодов.
func (s *Store) UpdatePayment(id int64, name string, amount *int64, currency, period string, firstDue string, userID int64) error {
	res, err := s.db.Exec(`UPDATE recurring_payments SET
		 name           = COALESCE(NULLIF(?, ''), name),
		 amount         = COALESCE(?, amount),
		 currency       = COALESCE(NULLIF(?, ''), currency),
		 period         = COALESCE(NULLIF(?, ''), period),
		 first_due_date = COALESCE(NULLIF(?, ''), first_due_date),
		 user_id        = ?
		WHERE id = ?`,
		name, amount, currency, period, firstDue, nullInt64(userID), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// SetPaymentUser меняет ответственного платежа; 0 снимает привязку.
func (s *Store) SetPaymentUser(id, userID int64) error {
	res, err := s.db.Exec(`UPDATE recurring_payments SET user_id = ? WHERE id = ?`, nullInt64(userID), id)
	if err != nil {
		return err
	}
	return affected(res)
}

// DeactivatePayment прекращает платёж и его напоминания, сохраняя историю
// оплат — ровно как требует кейс.
func (s *Store) DeactivatePayment(id int64) error {
	res, err := s.db.Exec(`UPDATE recurring_payments SET active = 0 WHERE id = ? AND active = 1`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) ReactivatePayment(id int64) error {
	res, err := s.db.Exec(`UPDATE recurring_payments SET active = 1 WHERE id = ? AND active = 0`, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// ── периоды ────────────────────────────────────────────────────────────────

// AddOccurrences идемпотентно добавляет периоды платежа.
func (s *Store) AddOccurrences(paymentID int64, dates []domain.Day) error {
	if len(dates) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO occurrences (payment_id, due_date) VALUES (?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, d := range dates {
		if _, err := stmt.Exec(paymentID, d.String()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SyncOccurrences доводит набор периодов активного платежа до актуального:
// создаёт недостающие от первого срока и на monthsAhead месяцев вперёд.
//
// Прошедшие периоды создаются намеренно: только так платёж, добавленный
// задним числом, становится просроченным, а напоминание по нему уходит сразу.
// INSERT OR IGNORE и UNIQUE(payment_id, due_date) делают операцию
// идемпотентной, поэтому существующие отметки об оплате не затираются.
func (s *Store) SyncOccurrences(p *domain.RecurringPayment, today domain.Day, monthsAhead int) error {
	if !p.Active {
		return nil
	}

	var last sql.NullString
	err := s.db.QueryRow(`SELECT max(due_date) FROM occurrences WHERE payment_id = ?`, p.ID).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	limit := today.AddMonths(monthsAhead)
	var pending []domain.Day
	for n := 0; n < maxPeriods; n++ {
		d := p.DueDate(n)
		if d.After(limit) {
			break
		}
		if last.Valid && !d.After(day(last.String)) {
			continue // период уже есть
		}
		pending = append(pending, d)
	}
	return s.AddOccurrences(p.ID, pending)
}

// DropFutureOccurrences убирает неоплаченные периоды начиная со today — их
// даты теряют смысл после изменения срока или периодичности шаблона. Оплаченные
// и прошлые периоды не затрагиваются: история сохраняется.
func (s *Store) DropFutureOccurrences(paymentID int64, from domain.Day) error {
	_, err := s.db.Exec(`DELETE FROM occurrences WHERE payment_id = ? AND paid_at IS NULL AND due_date >= ?`,
		paymentID, from.String())
	return err
}

// SetOccurrenceDate переносит срок отдельного периода. Флаг напоминания
// сбрасывается: с новой датой напоминание снова актуально.
func (s *Store) SetOccurrenceDate(id int64, due domain.Day) error {
	res, err := s.db.Exec(`UPDATE occurrences SET due_date = ?, reminder_sent = 0 WHERE id = ?`,
		due.String(), id)
	if err != nil {
		return err
	}
	return affected(res)
}

const occSelect = `
SELECT o.id, o.payment_id, p.name, p.amount, p.currency, p.period,
       o.due_date, o.paid_at, o.reminder_sent, p.active, p.user_id, u.login
FROM occurrences o
JOIN recurring_payments p ON p.id = o.payment_id
LEFT JOIN users u ON u.id = p.user_id`

func scanOccurrence(scan func(...any) error) (domain.Occurrence, error) {
	var (
		o        domain.Occurrence
		amount   int64
		sent     int
		active   int
		userID   sql.NullInt64
		userName sql.NullString
		due      string
		paid     sql.NullString
	)
	err := scan(&o.ID, &o.PaymentID, &o.PaymentName, &amount, &o.Currency, &o.Period,
		&due, &paid, &sent, &active, &userID, &userName)
	if err != nil {
		return o, err
	}
	o.Amount, o.DueDate = domain.Amount(amount), day(due)
	o.ReminderSent, o.Active = sent == 1, active == 1
	o.UserID, o.UserName = userID.Int64, userName.String
	if paid.Valid {
		d := day(paid.String)
		o.PaidAt = &d
	}
	return o, nil
}

func (s *Store) ListOccurrences(paymentID int64) ([]domain.Occurrence, error) {
	return s.ListOccurrencesFiltered(paymentID, -1)
}

// ListOccurrencesFiltered отдаёт периоды платежей: по шаблону (paymentID = 0 —
// любые) и по ответственному. userID = -1 снимает фильтр по пользователю,
// userID = 0 оставляет только платежи без ответственного.
func (s *Store) ListOccurrencesFiltered(paymentID, userID int64) ([]domain.Occurrence, error) {
	var where []string
	args := []any{}
	if paymentID > 0 {
		where = append(where, `o.payment_id = ?`)
		args = append(args, paymentID)
	}
	switch {
	case userID > 0:
		where = append(where, `p.user_id = ?`)
		args = append(args, userID)
	case userID == 0:
		where = append(where, `p.user_id IS NULL`)
	}

	q := occSelect
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	rows, err := s.db.Query(q+` ORDER BY o.due_date, o.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Occurrence
	for rows.Next() {
		o, err := scanOccurrence(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) OccurrenceByID(id int64) (*domain.Occurrence, error) {
	o, err := scanOccurrence(s.db.QueryRow(occSelect+` WHERE o.id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// SetPaid выставляет либо снимает отметку об оплате конкретного периода.
// nil снимает отметку и очищает дату — просрочка пересчитается автоматически.
func (s *Store) SetPaid(id int64, paidAt *domain.Day) error {
	var value any
	if paidAt != nil {
		value = paidAt.String()
	}
	res, err := s.db.Exec(`UPDATE occurrences SET paid_at = ? WHERE id = ?`, value, id)
	if err != nil {
		return err
	}
	return affected(res)
}

// MarkReminderSent возвращает true, только если напоминание ещё не уходило.
// Условие в одном UPDATE — атомарная защита от дублей на уровне хранилища.
func (s *Store) MarkReminderSent(id int64) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE occurrences SET reminder_sent = 1 WHERE id = ? AND reminder_sent = 0`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClearReminder снимает флаг отправленного напоминания — возвращает в очередь,
// если доставка не удалась.
func (s *Store) ClearReminder(id int64) error {
	_, err := s.db.Exec(`UPDATE occurrences SET reminder_sent = 0 WHERE id = ? AND reminder_sent = 1`, id)
	return err
}

// ── отозванные сессии ──────────────────────────────────────────────────────

// RevokeToken вносит идентификатор токена в чёрный список: logout делает
// Bearer-токен непригодным до его естественного истечения.
func (s *Store) RevokeToken(jti string, expiresAt time.Time) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO revoked_tokens (jti, revoked_at, expires_at) VALUES (?,?,?)`,
		jti, now(), expiresAt.UTC().Format(time.RFC3339))
	return err
}

func (s *Store) IsTokenRevoked(jti string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM revoked_tokens WHERE jti = ?`, jti).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// PurgeExpiredTokens убирает записи об истёкших сессиях, чтобы таблица не росла.
func (s *Store) PurgeExpiredTokens(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM revoked_tokens WHERE expires_at < ?`, now.UTC().Format(time.RFC3339))
	return err
}

// HasPayments — нужна, чтобы демо-данные загружались однократно.
func (s *Store) HasPayments() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM recurring_payments`).Scan(&n)
	return n > 0, err
}

// ── Telegram-чаты ─────────────────────────────────────────────────────────

// Telegram chat_id добавляются командой /start и живут в базе: напоминания
// уходят всем подписавшимся, а список переживает перезапуск.

func (s *Store) AddTelegramChat(chatID string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO telegram_chats (chat_id, created_at) VALUES (?,?)`,
		chatID, now())
	return err
}

func (s *Store) RemoveTelegramChat(chatID string) error {
	res, err := s.db.Exec(`DELETE FROM telegram_chats WHERE chat_id = ?`, chatID)
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) ListTelegramChats() ([]string, error) {
	rows, err := s.db.Query(`SELECT chat_id FROM telegram_chats ORDER BY created_at, chat_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// HasTelegramChats — нужна, чтобы автоподписка чата из .env срабатывала
// только на пустой базе.
func (s *Store) HasTelegramChats() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM telegram_chats`).Scan(&n)
	return n > 0, err
}

// ── служебное ───────────────────────────────────────────────────────────────

const maxPeriods = 480 // 40 лет monthly — защита от бесконечного цикла

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// nullInt64 превращает 0 в NULL: так «не назначен» и «нет ответственного»
// сохраняются одинаково и при удалении пользователя ссылка обнуляется сама.
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func day(s string) domain.Day {
	d, _ := domain.ParseDay(s)
	return d
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func uniqueViolation(err error) bool {
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
