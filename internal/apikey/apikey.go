package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

const (
	keyPrefix    = "fsk_"
	secretBytes  = 32
	displayChars = 12
)

type Issued struct {
	Plain   string
	Hash    []byte
	Display string
}

func Generate() (Issued, error) {
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return Issued{}, err
	}

	plain := keyPrefix + base64.RawURLEncoding.EncodeToString(secret)

	return Issued{Plain: plain, Hash: Hash(plain), Display: plain[:displayChars]}, nil
}

func Hash(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))

	return sum[:]
}

func WellFormed(plain string) bool {
	secret, ok := strings.CutPrefix(plain, keyPrefix)
	if !ok {
		return false
	}

	decoded, err := base64.RawURLEncoding.DecodeString(secret)

	return err == nil && len(decoded) == secretBytes
}
