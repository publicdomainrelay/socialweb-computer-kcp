package guestagent

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
)

const maxPlaintext = 1 << 16

func seal(recipient *age.X25519Recipient, plaintext []byte) (string, error) {
	var out bytes.Buffer
	armored := armor.NewWriter(&out)
	w, err := age.Encrypt(armored, recipient)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(plaintext); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	if err := armored.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func open(identity *age.X25519Identity, ciphertext string) ([]byte, error) {
	r, err := age.Decrypt(armor.NewReader(strings.NewReader(ciphertext)), identity)
	if err != nil {
		return nil, err
	}
	plaintext, err := io.ReadAll(io.LimitReader(r, maxPlaintext+1))
	if err != nil {
		return nil, err
	}
	if len(plaintext) > maxPlaintext {
		return nil, fmt.Errorf("guestagent: the sealed body is larger than %d bytes", maxPlaintext)
	}
	return plaintext, nil
}
