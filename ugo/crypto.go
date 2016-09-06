package ugo

import (
	"crypto/aes"
	"crypto/cipher"
)

type StreamCrypto interface {
	Encrypt(dst, src []byte)
	Decrypt(dst, src []byte)
}

type AESStreamCrypto struct {
	key []byte
	iv  []byte
	enc cipher.Stream
	dec cipher.Stream
}

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
