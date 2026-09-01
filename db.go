package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// ==== Account storage ====
// Naya account banate hi 10,000 coins + 30 diamonds mil jate hain. Password
// kabhi bhi plain-text save nahi hota — bcrypt hash rakha jata hai (yeh
// security ke liye zaroori hai; asal password kisi ko bhi, khud Anthropic ko
// bhi, kabhi nazar nahi aata). Email hi login ke liye "Gmail" field hai.

const SignupBonusCoins = 10000
const SignupBonusDiamonds = 30

type Account struct {
	ID        string
	Email     string
	Coins     int64
	Diamonds  int64
	CreatedAt time.Time
}

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	schema := `
	CREATE TABLE IF NOT EXISTS accounts (
		id TEXT PRIMARY KEY,
		email TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		coins INTEGER NOT NULL DEFAULT 0,
		diamonds INTEGER NOT NULL DEFAULT 0,
		token TEXT,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_accounts_token ON accounts(token);
	`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// SignUp — naya account banata hai, signup bonus deta hai, session token wapis karta hai.
func (s *Store) SignUp(email, password string) (Account, string, error) {
	email = normalizeEmail(email)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return Account{}, "", err
	}
	id, err := randomHex(12)
	if err != nil {
		return Account{}, "", err
	}
	token, err := randomHex(24)
	if err != nil {
		return Account{}, "", err
	}
	now := time.Now().UTC()
	_, err = s.db.Exec(
		`INSERT INTO accounts (id, email, password_hash, coins, diamonds, token, created_at) VALUES (?,?,?,?,?,?,?)`,
		id, email, string(hash), SignupBonusCoins, SignupBonusDiamonds, token, now.Format(time.RFC3339),
	)
	if err != nil {
		return Account{}, "", errors.New("yeh email pehle se registered hai")
	}
	return Account{ID: id, Email: email, Coins: SignupBonusCoins, Diamonds: SignupBonusDiamonds, CreatedAt: now}, token, nil
}

// Login — email/password verify karta hai aur naya session token deta hai.
func (s *Store) Login(email, password string) (Account, string, error) {
	email = normalizeEmail(email)
	var id, hash, createdAt string
	var coins, diamonds int64
	row := s.db.QueryRow(`SELECT id, password_hash, coins, diamonds, created_at FROM accounts WHERE email = ?`, email)
	if err := row.Scan(&id, &hash, &coins, &diamonds, &createdAt); err != nil {
		return Account{}, "", errors.New("email ya password ghalat hai")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return Account{}, "", errors.New("email ya password ghalat hai")
	}
	token, err := randomHex(24)
	if err != nil {
		return Account{}, "", err
	}
	if _, err := s.db.Exec(`UPDATE accounts SET token = ? WHERE id = ?`, token, id); err != nil {
		return Account{}, "", err
	}
	t, _ := time.Parse(time.RFC3339, createdAt)
	return Account{ID: id, Email: email, Coins: coins, Diamonds: diamonds, CreatedAt: t}, token, nil
}

// GetByToken — WebSocket connect hote hi token se account resolve karta hai.
func (s *Store) GetByToken(token string) (Account, error) {
	if token == "" {
		return Account{}, errors.New("token missing")
	}
	var id, email, createdAt string
	var coins, diamonds int64
	row := s.db.QueryRow(`SELECT id, email, coins, diamonds, created_at FROM accounts WHERE token = ?`, token)
	if err := row.Scan(&id, &email, &coins, &diamonds, &createdAt); err != nil {
		return Account{}, errors.New("invalid ya expired token — dobara login karein")
	}
	t, _ := time.Parse(time.RFC3339, createdAt)
	return Account{ID: id, Email: email, Coins: coins, Diamonds: diamonds, CreatedAt: t}, nil
}

func (s *Store) GetCoins(id string) (int64, error) {
	var coins int64
	err := s.db.QueryRow(`SELECT coins FROM accounts WHERE id = ?`, id).Scan(&coins)
	return coins, err
}

// AdjustCoins — coins mein +/- karta hai (bet deduct karne ke liye negative
// delta, jeetne par pot credit karne ke liye positive). Balance kabhi negative
// nahi hone deta.
func (s *Store) AdjustCoins(id string, delta int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var coins int64
	if err := tx.QueryRow(`SELECT coins FROM accounts WHERE id = ?`, id).Scan(&coins); err != nil {
		return 0, err
	}
	newCoins := coins + delta
	if newCoins < 0 {
		return 0, errors.New("insufficient coins")
	}
	if _, err := tx.Exec(`UPDATE accounts SET coins = ? WHERE id = ?`, newCoins, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newCoins, nil
}

// AdjustDiamonds — diamonds mein +/- karta hai (extra dice roll khareedne ke
// liye negative delta). Balance kabhi negative nahi hone deta.
func (s *Store) AdjustDiamonds(id string, delta int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var diamonds int64
	if err := tx.QueryRow(`SELECT diamonds FROM accounts WHERE id = ?`, id).Scan(&diamonds); err != nil {
		return 0, err
	}
	newDiamonds := diamonds + delta
	if newDiamonds < 0 {
		return 0, errors.New("insufficient diamonds")
	}
	if _, err := tx.Exec(`UPDATE accounts SET diamonds = ? WHERE id = ?`, newDiamonds, id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newDiamonds, nil
}
