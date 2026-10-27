// Package secure holds the small cryptographic helpers Kaku relies on:
// AES-256-GCM for stored secrets (cards, delivered content, payment keys),
// keyed hashes for de-duplication, Argon2id for passwords and random tokens.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

// Box encrypts and hashes secrets with keys derived from one master key.
type Box struct {
	aead    cipher.AEAD
	hashKey []byte
	macKey  []byte
}

// ParseMasterKey accepts 32 bytes as base64 (std or url) or hex.
func ParseMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, dec := range []func(string) ([]byte, error){
		hex.DecodeString,
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
	} {
		if b, err := dec(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("KAKU_MASTER_KEY 需为 32 字节（64 位十六进制或 base64）")
}

// NewMasterKey returns a fresh key as base64.
func NewMasterKey() string {
	return base64.StdEncoding.EncodeToString(RandomBytes(32))
}

// NewBox derives the encryption, hashing and signing keys.
func NewBox(master []byte) (*Box, error) {
	if len(master) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	derive := func(info string) []byte {
		out := make([]byte, 32)
		r := hkdf.New(sha256.New, master, nil, []byte("kaku "+info))
		if _, err := r.Read(out); err != nil {
			panic(err)
		}
		return out
	}
	block, err := aes.NewCipher(derive("aes-gcm"))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, hashKey: derive("hash"), macKey: derive("mac")}, nil
}

// Seal encrypts plaintext; the result is base64(nonce|ciphertext).
func (b *Box) Seal(plain string) string {
	if plain == "" {
		return ""
	}
	nonce := RandomBytes(b.aead.NonceSize())
	out := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(out)
}

// Open decrypts a value made by Seal.
func (b *Box) Open(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("密文格式不正确")
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("解密失败（主密钥是否变更？）")
	}
	return string(plain), nil
}

// MustOpen decrypts or returns a placeholder (for display).
func (b *Box) MustOpen(enc string) string {
	s, err := b.Open(enc)
	if err != nil {
		return "［无法解密］"
	}
	return s
}

// Hash is a keyed hash for de-duplication and lookups: equal inputs match,
// but a leaked database does not reveal guessable secrets.
func (b *Box) Hash(s string) string {
	m := hmac.New(sha256.New, b.hashKey)
	m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}

// Sign returns an HMAC for tokens (sessions, order access).
func (b *Box) Sign(parts ...string) string {
	m := hmac.New(sha256.New, b.macKey)
	m.Write([]byte(strings.Join(parts, "\x00")))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a token made by Sign.
func (b *Box) Verify(sig string, parts ...string) bool {
	return subtle.ConstantTimeCompare([]byte(sig), []byte(b.Sign(parts...))) == 1
}

// RandomBytes returns n random bytes.
func RandomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// RandomToken returns a url-safe random token of n bytes of entropy.
func RandomToken(n int) string {
	return base64.RawURLEncoding.EncodeToString(RandomBytes(n))
}

const digits = "0123456789"

// RandomDigits returns n random decimal digits.
func RandomDigits(n int) string {
	b := RandomBytes(n)
	for i := range b {
		b[i] = digits[int(b[i])%10]
	}
	return string(b)
}

// Argon2id parameters (OWASP: m=19 MiB, t=2, p=1).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// HashPassword returns an encoded Argon2id hash.
func HashPassword(pw string) string {
	salt := RandomBytes(16)
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// CheckPassword verifies pw against an encoded Argon2id hash.
func CheckPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || m > 256*1024 || t > 10 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
