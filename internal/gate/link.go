package gate

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Ошибки проверки ссылки.
var (
	// ErrLinkBadSignature — подпись не сходится. Значит ссылку подделали или
	// она от другого ключа.
	ErrLinkBadSignature = errors.New("подпись ссылки одобрения не сходится")
	// ErrLinkExpired — срок истёк.
	ErrLinkExpired = errors.New("срок ссылки одобрения истёк")
	// ErrLinkMalformed — ссылка не разбирается.
	ErrLinkMalformed = errors.New("ссылка одобрения не разбирается")
)

// Link — что именно подписано.
//
// Привязка к рану и шагу обязательна: подпись без привязки была бы
// «одобрите что угодно», а подпись только к сроку — «одобрите любой гейт до
// этого момента». И то и другое — дыра.
type Link struct {
	RunID   string    `json:"r"`
	StepID  string    `json:"s"`
	Expires time.Time `json:"e"`
}

// Signer — ключ подписи ссылок одобрения.
//
// Ключ живёт в файле рядом с ранами, потому что ссылка должна пережить
// перезапуск демона: человек получил ссылку, демон рестартнул по таймауту
// или systemd, и ссылка должна остаться рабочей.
type Signer struct {
	key []byte
}

// NewSigner — новый ключ из криптостойких случайных байт.
func NewSigner() (*Signer, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("ключ подписи: %w", err)
	}
	return &Signer{key: key}, nil
}

// LoadSigner — читает ключ из файла или создаёт новый.
//
// Права 0600: ключ — это и есть право одобрять гейты. Файл создаётся с
// umask-защитой, и если ключ уже существует, но доступен кому-то ещё, это
// ошибка, а не повод молча перезаписать.
func LoadSigner(path string) (*Signer, error) {
	if path == "" {
		return nil, errors.New("путь к ключу подписи")
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		key, derr := base64.RawURLEncoding.DecodeString(string(raw))
		if derr != nil || len(key) < 32 {
			return nil, fmt.Errorf("ключ подписи %s повреждён", path)
		}
		return &Signer{key: key}, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("ключ подписи %s: %w", path, err)
	}
	s, err := NewSigner()
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("каталог для ключа подписи: %w", err)
		}
	}
	encoded := base64.RawURLEncoding.EncodeToString(s.key)
	if err := secureWriteFile(path, []byte(encoded)); err != nil {
		return nil, err
	}
	return s, nil
}

// Sign — токен ссылки.
//
// Формат: base64url(полезная нагрузка) + "." + base64url(HMAC-SHA256).
// Точка и base64url выбраны потому, что токен целиком вставляется в URL и в
// командную строку: ни один символ не требует экранирования.
func (s *Signer) Sign(l Link) (string, error) {
	if s == nil || len(s.key) == 0 {
		return "", errors.New("ключ подписи не задан")
	}
	if l.RunID == "" {
		return "", errors.New("ссылка одобрения без run_id")
	}
	payload, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify — проверка токена.
//
// Сначала подпись, потом срок. Обратный порядок дал бы «ссылка просрочена»
// для подделки, а это сообщение вводит в заблуждение: подделка не истекла, её
// не должно существовать вовсе.
func (s *Signer) Verify(token string, now time.Time) (Link, error) {
	var l Link
	if s == nil || len(s.key) == 0 {
		return l, errors.New("ключ подписи не задан")
	}
	parts := splitToken(token)
	if len(parts) != 2 {
		return l, ErrLinkMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return l, ErrLinkMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return l, ErrLinkMalformed
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return l, ErrLinkBadSignature
	}
	if err := json.Unmarshal(payload, &l); err != nil {
		return l, ErrLinkMalformed
	}
	if l.Expires.IsZero() {
		return l, ErrLinkMalformed
	}
	if now.After(l.Expires) {
		return l, ErrLinkExpired
	}
	return l, nil
}

// splitToken — разделение по последней точке: base64url не содержит точки,
// так что точка ровно одна, но находить её лучше с конца — на случай
// нестандартного ввода.
func splitToken(token string) []string {
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			return []string{token[:i], token[i+1:]}
		}
	}
	return nil
}
