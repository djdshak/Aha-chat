package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

var appDB *sql.DB

type MessageRow struct {
	ID        int64  `json:"id"`
	MsgID     string `json:"msg_id"`
	FromUser  string `json:"from_user"`
	ToUser    string `json:"to_user"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"created_at"`
}

func InitDB(dbPath string) error {
	if dbPath == "" {
		dbPath = "aha_chat.db"
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return fmt.Errorf("ping sqlite: %w", err)
	}

	pragmas := []string{
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA synchronous = NORMAL;`,
		`PRAGMA busy_timeout = 5000;`,
	}

	for _, q := range pragmas {
		if _, err := db.Exec(q); err != nil {
			_ = db.Close()
			return fmt.Errorf("exec pragma %q: %w", q, err)
		}
	}

	schema := []string{
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			msg_id TEXT NOT NULL UNIQUE,
			from_user TEXT NOT NULL,
			to_user TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_messages_to_user_id
			ON messages(to_user, id);`,
		`CREATE INDEX IF NOT EXISTS idx_messages_pair_id
			ON messages(from_user, to_user, id);`,
		`CREATE TABLE IF NOT EXISTS users (
    		id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password_hash BLOB NOT NULL,
			created_at INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS tokens (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		);`,
		`CREATE INDEX IF NOT EXISTS idx_tokens_user_id ON tokens(user_id);`,
		`CREATE INDEX IF NOT EXISTS idx_tokens_expires_at ON tokens(expires_at);`,
	}

	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			_ = db.Close()
			return fmt.Errorf("exec schema: %w", err)
		}
	}

	appDB = db
	log.Printf("InitDB ok: appDB=%p", appDB)
	return nil
}

func CloseDB() error {
	if appDB == nil {
		return nil
	}
	return appDB.Close()
}

func StoreTextMessage(ctx context.Context, msgID, fromUser,
	toUser, text string, createdAt int64) (rowID int64,
	inserted bool, err error) {
	if appDB == nil {
		return 0, false, errors.New("db not initialized")
	}

	res, err := appDB.ExecContext(ctx,
		`INSERT OR IGNORE INTO messages (msg_id, from_user, to_user, text, created_at)
	VALUES(?,?,?,?,?)`, msgID, fromUser, toUser, text, createdAt)
	if err != nil {
		return 0, false, fmt.Errorf("intsert message: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("rows affected %w", err)
	}

	if affected == 0 {
		if err := appDB.QueryRowContext(ctx, `SELECT id FROM messages 
			WHERE msg_id = ?`, msgID).Scan(&rowID); err != nil {
			return 0, false, fmt.Errorf("query existing row id: %w", err)
		}
		return rowID, false, nil
	}

	rowID, err = res.LastInsertId()
	if err != nil {
		// 兜底查一次
		if err2 := appDB.QueryRowContext(ctx, `SELECT id FROM messages WHERE msg_id = ?`, msgID).Scan(&rowID); err2 != nil {
			return 0, true, fmt.Errorf("lastinsertid failed (%v), fallback query failed: %w", err, err2)
		}
	}
	return rowID, true, nil
}

// QueryConversation 查询 me<->peer 的会话历史
// beforeID > 0 时：查询 id < beforeID 的更早消息
// 返回按 id ASC（旧 -> 新）

func QueryConversation(ctx context.Context, me, peer string, beforeID int64, limit int) ([]MessageRow, error) {
	if appDB == nil {
		return nil, errors.New("db not initialized")
	}

	limit = normalizeLimit(limit, 20, 1, 200)

	var (
		rows *sql.Rows
		err  error
	)

	if beforeID > 0 {
		rows, err = appDB.QueryContext(ctx, `
			SELECT id, msg_id, from_user, to_user, text, created_at
			FROM messages
			WHERE (
				(from_user = ? AND to_user = ?)
				OR
				(from_user = ? AND to_user = ?)
			)
			AND id < ?
			ORDER BY id DESC
			LIMIT ?
		`, me, peer, peer, me, beforeID, limit)
	} else {
		rows, err = appDB.QueryContext(ctx, `
			SELECT id, msg_id, from_user, to_user, text, created_at
			FROM messages
			WHERE (
				(from_user = ? AND to_user = ?)
				OR
				(from_user = ? AND to_user = ?)
			)
			ORDER BY id DESC
			LIMIT ?
		`, me, peer, peer, me, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query conversation: %w", err)
	}
	defer rows.Close()

	items := make([]MessageRow, 0, limit)
	for rows.Next() {
		var m MessageRow
		if err := rows.Scan(&m.ID, &m.MsgID, &m.FromUser, &m.ToUser, &m.Text, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan conversation row: %w", err)
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversation rows err: %w", err)
	}
	reverseMessages(items) // DESC -> ASC
	return items, nil
}

func QuerySyncInboxAfterID(ctx context.Context, user string, afterID int64, limit int) ([]MessageRow, error) {
	if appDB == nil {
		return nil, errors.New("db not initialized")
	}
	limit = normalizeLimit(limit, 100, 1, 500)
	if afterID < 0 {
		afterID = 0
	}

	rows, err := appDB.QueryContext(ctx, `
		SELECT id, msg_id, from_user, to_user, text, created_at
		FROM messages
		WHERE to_user = ? AND id > ?
		ORDER BY id ASC
		LIMIT ?
	`, user, afterID, limit)

	if err != nil {
		return nil, fmt.Errorf("query sync inbox: %w", err)
	}
	defer rows.Close()

	items := make([]MessageRow, 0, limit)
	for rows.Next() {
		var m MessageRow
		if err := rows.Scan(&m.ID, &m.MsgID, &m.FromUser, &m.ToUser, &m.Text, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan sync row: %w", err)
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync rows err: %w", err)
	}

	return items, nil
}

func normalizeLimit(v, def, min, max int) int {
	if v <= 0 {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func reverseMessages(a []MessageRow) {
	for i, j := 0, len(a)-1; i < j; i, j = i+1, j-1 {
		a[i], a[j] = a[j], a[i]
	}
}

// 注册和登陆 tokens

type AuthUser struct {
	ID       int64
	Username string
}

func CreateUser(ctx context.Context, username, password string) error {
	if appDB == nil {
		return errors.New("db not initialized")
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("empty username")
	}
	if len(password) < 6 {
		return errors.New("password too short (min 6)")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}

	_, err = appDB.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at)
        VALUES (?, ?, ?)`,
		username, hash, time.Now().Unix(),
	)

	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

func AuthenticateUser(ctx context.Context, username, password string) (AuthUser, error) {
	if appDB == nil {
		return AuthUser{}, errors.New("db not initialized")
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return AuthUser{}, errors.New("empty username")
	}

	var (
		id   int64
		hash []byte
	)

	err := appDB.QueryRowContext(ctx,
		`SELECT id, password_hash FROM users WHERE username = ?`,
		username).Scan(&id, &hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AuthUser{}, errors.New("invalid username or password")
		}
		return AuthUser{}, fmt.Errorf("query user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
		return AuthUser{}, errors.New("invalid username or password")
	}

	return AuthUser{ID: id, Username: username}, nil
}

func IssueToken(ctx context.Context, userID int64, ttl time.Duration) (token string,
	expiresAt int64, err error) {
	if appDB == nil {
		return "", 0, errors.New("db not initialized")
	}
	token, err = newTokenString(32)
	if err != nil {
		return "", 0, err
	}

	now := time.Now().Unix()
	expiresAt = now + int64(ttl.Seconds())

	_, err = appDB.ExecContext(ctx,
		`INSERT INTO tokens (token, user_id, created_at, expires_at) 
		VALUES (?, ?, ?, ?)`,
		token, userID, now, expiresAt)
	if err != nil {
		return "", 0, fmt.Errorf("insert token: %w", err)
	}
	return token, expiresAt, nil
}

func UsernameByToken(ctx context.Context, token string) (string, error) {
	if appDB == nil {
		return "", errors.New("db not initialized")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("empty token")
	}

	var (
		username  string
		expiresAt int64
	)

	err := appDB.QueryRowContext(ctx,
		`SELECT u.username, t.expires_at
		 FROM tokens t
		 JOIN users u ON u.id = t.user_id
		 WHERE t.token = ?`,
		token).Scan(&username, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errors.New("invalid token")
		}
		return "", fmt.Errorf("query token: %w", err)
	}

	if time.Now().Unix() > expiresAt {
		_, _ = appDB.ExecContext(ctx,
			`DELETE FROM tokens WHERE token = ?`, token)
		return "", errors.New("token expired")
	}
	return username, nil
}

func newTokenString(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
