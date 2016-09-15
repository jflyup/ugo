package ugo

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rc4"
)

type streamCrypto interface {
	Encrypt(dst, src []byte)
	Decrypt(dst, src []byte)
}

type rc4StreamCrypto struct {
	key []byte
}

func newRC4Crypto(key []byte) *rc4StreamCrypto {

	return &rc4StreamCrypto{
		key: key,
	}
}

func (c *rc4StreamCrypto) Encrypt(dst, src []byte) {
	cipher, err := rc4.NewCipher(c.key)
	if err != nil {
		return
	}
	cipher.XORKeyStream(dst, src)
}

func (c *rc4StreamCrypto) Decrypt(dst, src []byte) {
	cipher, err := rc4.NewCipher(c.key)
	if err != nil {
		return
	}
	cipher.XORKeyStream(dst, src)
}

type AESStreamCrypto struct {
	key []byte
	iv  []byte
	enc cipher.Stream
	dec cipher.Stream
}

// it's not safe for concurrent use!
func newAESStreamCrypto(key []byte, iv []byte) (c *AESStreamCrypto, err error) {
	// the IV must have the same length as the block
	aesBlock, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, err
	}

	c = &AESStreamCrypto{
		iv:  iv,
		key: key,
		enc: cipher.NewCFBEncrypter(aesBlock, iv),
		dec: cipher.NewCFBDecrypter(aesBlock, iv),
	}

	return c, nil
}

func (c *AESStreamCrypto) Encrypt(dst, src []byte) {
	c.enc.XORKeyStream(dst, src)
}

func (c *AESStreamCrypto) Decrypt(dst, src []byte) {
	c.dec.XORKeyStream(dst, src)
}
