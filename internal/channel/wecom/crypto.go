package wecom

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
)

// Crypt is WeCom's callback message encryption (the scheme of its
// WXBizMsgCrypt samples): AES-256-CBC with the key from the 43-character
// EncodingAESKey and the key's first 16 bytes as IV, PKCS#7 padding to 32
// bytes, and a plaintext of 16 random bytes, the message length (4 bytes,
// big-endian), the message and the receiver id (the corp id). Requests are
// signed with SHA-1 over the sorted token, timestamp, nonce and ciphertext.
type Crypt struct {
	token      string
	key        []byte
	receiverID string
}

const padBlock = 32

func NewCrypt(token, encodingAESKey, receiverID string) (*Crypt, error) {
	if len(encodingAESKey) != 43 {
		return nil, errors.New("wecom: the EncodingAESKey must be 43 characters")
	}
	key, err := base64.StdEncoding.DecodeString(encodingAESKey + "=")
	if err != nil || len(key) != 32 {
		return nil, errors.New("wecom: the EncodingAESKey is not valid base64")
	}
	return &Crypt{token: token, key: key, receiverID: receiverID}, nil
}

// Signature is the msg_signature of a request.
func (c *Crypt) Signature(timestamp, nonce, encrypted string) string {
	parts := []string{c.token, timestamp, nonce, encrypted}
	slices.Sort(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

// Verify checks a request's msg_signature.
func (c *Crypt) Verify(signature, timestamp, nonce, encrypted string) bool {
	return subtle.ConstantTimeCompare([]byte(signature), []byte(c.Signature(timestamp, nonce, encrypted))) == 1
}

// Decrypt opens a message and checks it was meant for this receiver.
func (c *Crypt) Decrypt(encrypted string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return nil, errors.New("wecom: ciphertext has a bad length")
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, c.key[:aes.BlockSize]).CryptBlocks(out, raw)
	n := int(out[len(out)-1])
	if n == 0 || n > padBlock || n > len(out) {
		return nil, errors.New("wecom: bad padding")
	}
	out = out[:len(out)-n]
	if len(out) < 20 {
		return nil, errors.New("wecom: message too short")
	}
	size := int(binary.BigEndian.Uint32(out[16:20]))
	if size > len(out)-20 {
		return nil, errors.New("wecom: bad message length")
	}
	msg, receiver := out[20:20+size], string(out[20+size:])
	if c.receiverID != "" && subtle.ConstantTimeCompare([]byte(receiver), []byte(c.receiverID)) != 1 {
		return nil, errors.New("wecom: message is for another receiver")
	}
	return msg, nil
}

// Encrypt seals a message (for replies, tests and fakes).
func (c *Crypt) Encrypt(msg []byte) string {
	plain := make([]byte, 20, 20+len(msg)+len(c.receiverID)+padBlock)
	_, _ = rand.Read(plain[:16])
	binary.BigEndian.PutUint32(plain[16:20], uint32(len(msg)))
	plain = append(append(plain, msg...), c.receiverID...)
	n := padBlock - len(plain)%padBlock
	for range n {
		plain = append(plain, byte(n))
	}
	block, _ := aes.NewCipher(c.key)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, c.key[:aes.BlockSize]).CryptBlocks(out, plain)
	return base64.StdEncoding.EncodeToString(out)
}
