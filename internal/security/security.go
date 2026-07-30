package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory  = 64 * 1024
	argonTime    = 3
	argonThreads = 2
	argonKeyLen  = 32
)

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", errors.New("password must be at least 12 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

func CheckPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	var memory, iterations uint64
	var threads uint64
	for _, param := range strings.Split(parts[3], ",") {
		pair := strings.SplitN(param, "=", 2)
		if len(pair) != 2 {
			return false
		}
		value, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil {
			return false
		}
		switch pair[0] {
		case "m":
			memory = value
		case "t":
			iterations = value
		case "p":
			threads = value
		}
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	expected, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || memory < 8 || iterations < 1 || threads < 1 ||
		memory > 1024*1024 || iterations > 20 || threads > 16 || len(expected) < 16 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(memory), uint8(threads), uint32(len(expected)))
	return hmac.Equal(actual, expected)
}

func Token(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func Digest(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

type Cipher struct{ aead cipher.AEAD }

func NewCipher(secret string) (*Cipher, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(plain string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (c *Cipher) Decrypt(data []byte) (string, error) {
	n := c.aead.NonceSize()
	if len(data) < n {
		return "", errors.New("encrypted value is truncated")
	}
	plain, err := c.aead.Open(nil, data[:n], data[n:], nil)
	return string(plain), err
}

func SignedToken(secret, value string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(value))
	return value + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func VerifySignedToken(secret, signed string) (string, bool) {
	idx := strings.LastIndexByte(signed, '.')
	if idx < 1 {
		return "", false
	}
	value, signature := signed[:idx], signed[idx+1:]
	expected := SignedToken(secret, value)
	return value, hmac.Equal([]byte(expected), []byte(signed)) && signature != ""
}
