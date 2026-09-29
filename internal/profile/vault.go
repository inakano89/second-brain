package profile

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"github.com/inakano89/second-brain/internal/config"
	"github.com/inakano89/second-brain/internal/crypto"
	"github.com/inakano89/second-brain/internal/database"
)

const (
	encPrefix = "enc1:"
	// kvWrapped holds VAULT_KEY encrypted with BACKUP_ENCRYPTION_KEY, so a database backup
	// can be read again with the backup key alone.
	kvWrapped = "profile.vault.wrapped"
)

// ErrLocked means the vault key is missing and sensitive items cannot be read.
var ErrLocked = errors.New("perfil: chave do cofre (VAULT_KEY) ausente ou diferente da usada para cifrar")

type vault struct {
	cfg *config.Config
	db  *database.DB

	mu      sync.Mutex
	wrapped string // backup key the current wrap was made with
}

// aead returns the cipher, creating VAULT_KEY on first use (or recovering it from the
// wrapped copy in the database when .env was lost but the backup key is known).
func (v *vault) aead(ctx context.Context) (cipher.AEAD, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	raw := strings.TrimSpace(v.cfg.Get("VAULT_KEY"))
	if raw == "" {
		if k := v.unwrap(ctx); k != "" {
			raw = k
		} else {
			raw = crypto.RandomToken(32)
		}
		if err := v.cfg.Update(map[string]string{"VAULT_KEY": raw}); err != nil {
			return nil, err
		}
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errors.New("perfil: VAULT_KEY inválida (esperado 64 caracteres hexadecimais)")
	}
	v.wrap(ctx, raw)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (v *vault) wrap(ctx context.Context, raw string) {
	bk := v.cfg.Get("BACKUP_ENCRYPTION_KEY")
	if bk == "" || bk == v.wrapped {
		return
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return
	}
	ct, err := seal(crypto.DeriveKey(bk, salt), []byte(raw))
	if err != nil {
		return
	}
	if v.db.KVSet(ctx, kvWrapped, base64.StdEncoding.EncodeToString(append(salt, ct...))) == nil {
		v.wrapped = bk
	}
}

func (v *vault) unwrap(ctx context.Context) string {
	bk := v.cfg.Get("BACKUP_ENCRYPTION_KEY")
	s, ok, _ := v.db.KVGet(ctx, kvWrapped)
	if bk == "" || !ok {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) < 16 {
		return ""
	}
	pt, err := open(crypto.DeriveKey(bk, b[:16]), b[16:])
	if err != nil {
		return ""
	}
	return string(pt)
}

func seal(key, pt []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, pt, nil), nil
}

func open(key, ct []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ct) < g.NonceSize() {
		return nil, errors.New("ciphertext curto")
	}
	return g.Open(nil, ct[:g.NonceSize()], ct[g.NonceSize():], nil)
}

func (v *vault) encrypt(ctx context.Context, s string) (string, error) {
	g, err := v.aead(ctx)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return encPrefix + base64.StdEncoding.EncodeToString(g.Seal(nonce, nonce, []byte(s), nil)), nil
}

// decrypt returns s unchanged when it is not encrypted.
func (v *vault) decrypt(ctx context.Context, s string) (string, error) {
	if !strings.HasPrefix(s, encPrefix) {
		return s, nil
	}
	g, err := v.aead(ctx)
	if err != nil {
		return "", err
	}
	b, err := base64.StdEncoding.DecodeString(s[len(encPrefix):])
	if err != nil || len(b) < g.NonceSize() {
		return "", ErrLocked
	}
	pt, err := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], nil)
	if err != nil {
		return "", ErrLocked
	}
	return string(pt), nil
}
