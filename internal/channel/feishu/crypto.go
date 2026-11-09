package feishu

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

// Decrypt opens an "encrypt" field of an event: base64 of a 16-byte IV
// followed by AES-256-CBC ciphertext (PKCS#7 padded), keyed with the
// SHA-256 of the app's Encrypt Key.
func Decrypt(encryptKey, encrypted string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, err
	}
	if len(raw) < 2*aes.BlockSize || len(raw)%aes.BlockSize != 0 {
		return nil, errors.New("feishu: encrypted event has a bad length")
	}
	key := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(raw)-aes.BlockSize)
	cipher.NewCBCDecrypter(block, raw[:aes.BlockSize]).CryptBlocks(out, raw[aes.BlockSize:])
	n := int(out[len(out)-1])
	if n == 0 || n > aes.BlockSize || n > len(out) {
		return nil, errors.New("feishu: encrypted event has bad padding")
	}
	for _, b := range out[len(out)-n:] {
		if int(b) != n {
			return nil, errors.New("feishu: encrypted event has bad padding")
		}
	}
	return out[:len(out)-n], nil
}

// Encrypt is Decrypt's inverse, for tests and fakes.
func Encrypt(encryptKey string, plain []byte) string {
	key := sha256.Sum256([]byte(encryptKey))
	block, _ := aes.NewCipher(key[:])
	n := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte(nil), plain...), make([]byte, n)...)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(n)
	}
	out := make([]byte, aes.BlockSize+len(padded))
	_, _ = rand.Read(out[:aes.BlockSize])
	cipher.NewCBCEncrypter(block, out[:aes.BlockSize]).CryptBlocks(out[aes.BlockSize:], padded)
	return base64.StdEncoding.EncodeToString(out)
}

// Signature is X-Lark-Signature: hex SHA-256 of timestamp, nonce, Encrypt
// Key and the raw body, concatenated.
func Signature(timestamp, nonce, encryptKey string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(timestamp + nonce + encryptKey))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
