// Package crypto provides password hashing (Argon2id), random tokens and
// streaming AES-256-GCM file encryption used by backups.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 32 * 1024 // KiB — friendly to 32-bit SBCs
	argonThreads = 2
	argonKeyLen  = 32
)

// HashPassword returns an encoded Argon2id hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks password against an encoded Argon2id hash.
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// RandomToken returns n random bytes hex-encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Sign returns hex(HMAC-SHA256(secret, msg)).
func Sign(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// Equal compares two strings in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---- Streaming AES-256-GCM ----
//
// Format: magic "SBBK1" | salt(16) | noncePrefix(4) | chunks...
// Each chunk: len(uint32 BE) | ciphertext. Nonce = noncePrefix | counter(uint64 BE).
// AAD = counter || finalFlag, preventing reordering and truncation.

var magic = []byte("SBBK1")

const chunkSize = 64 * 1024

// DeriveKey derives a 256-bit key from a passphrase and salt using Argon2id.
func DeriveKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, 32)
}

func aad(counter uint64, final bool) []byte {
	b := make([]byte, 9)
	binary.BigEndian.PutUint64(b, counter)
	if final {
		b[8] = 1
	}
	return b
}

func nonce(prefix []byte, counter uint64) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint64(n[4:], counter)
	return n
}

// Encrypt streams src into dst encrypted with passphrase.
func Encrypt(dst io.Writer, src io.Reader, passphrase string) error {
	if passphrase == "" {
		return errors.New("crypto: empty passphrase")
	}
	salt := make([]byte, 16)
	prefix := make([]byte, 4)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if _, err := rand.Read(prefix); err != nil {
		return err
	}
	block, err := aes.NewCipher(DeriveKey(passphrase, salt))
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if _, err := dst.Write(append(append(append([]byte{}, magic...), salt...), prefix...)); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	next := make([]byte, chunkSize)
	n, err := io.ReadFull(src, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	var counter uint64
	for {
		m, rerr := io.ReadFull(src, next)
		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			return rerr
		}
		final := m == 0
		ct := gcm.Seal(nil, nonce(prefix, counter), buf[:n], aad(counter, final))
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(ct)))
		if _, err := dst.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := dst.Write(ct); err != nil {
			return err
		}
		if final {
			return nil
		}
		counter++
		buf, next = next, buf
		n = m
	}
}

// Decrypt streams an encrypted backup from src into dst.
func Decrypt(dst io.Writer, src io.Reader, passphrase string) error {
	head := make([]byte, len(magic)+16+4)
	if _, err := io.ReadFull(src, head); err != nil {
		return fmt.Errorf("crypto: header: %w", err)
	}
	if string(head[:len(magic)]) != string(magic) {
		return errors.New("crypto: not a Second Brain backup")
	}
	salt := head[len(magic) : len(magic)+16]
	prefix := head[len(magic)+16:]
	block, err := aes.NewCipher(DeriveKey(passphrase, salt))
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	var counter uint64
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(src, hdr[:]); err != nil {
			return errors.New("crypto: truncated backup")
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size > chunkSize+uint32(gcm.Overhead()) {
			return errors.New("crypto: invalid chunk size")
		}
		ct := make([]byte, size)
		if _, err := io.ReadFull(src, ct); err != nil {
			return errors.New("crypto: truncated chunk")
		}
		// Try as non-final first, then final.
		pt, err := gcm.Open(nil, nonce(prefix, counter), ct, aad(counter, false))
		final := false
		if err != nil {
			pt, err = gcm.Open(nil, nonce(prefix, counter), ct, aad(counter, true))
			if err != nil {
				return errors.New("crypto: authentication failed (wrong key or corrupted file)")
			}
			final = true
		}
		if _, err := dst.Write(pt); err != nil {
			return err
		}
		if final {
			return nil
		}
		counter++
	}
}

// EncryptFile encrypts src path into dst path.
func EncryptFile(src, dst, passphrase string) error {
	return transformFile(src, dst, func(w io.Writer, r io.Reader) error { return Encrypt(w, r, passphrase) })
}

// DecryptFile decrypts src path into dst path.
func DecryptFile(src, dst, passphrase string) error {
	return transformFile(src, dst, func(w io.Writer, r io.Reader) error { return Decrypt(w, r, passphrase) })
}

func transformFile(src, dst string, fn func(io.Writer, io.Reader) error) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := fn(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
