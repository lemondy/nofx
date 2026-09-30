package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func testService(t *testing.T) *CryptoService {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &CryptoService{privateKey: key, publicKey: &key.PublicKey, dataKey: make([]byte, 32)}
}

func encryptedTestPayload(t *testing.T, cs *CryptoService, user string) *EncryptedPayload {
	t.Helper()
	ts := time.Now().Unix()
	aad, _ := json.Marshal(AADData{UserID: user, TS: ts, Purpose: "sensitive_data_encryption"})
	key := make([]byte, 32)
	rand.Read(key)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, gcm.NonceSize())
	rand.Read(iv)
	ciphertext := gcm.Seal(nil, iv, []byte("secret"), aad)
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, cs.publicKey, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return &EncryptedPayload{WrappedKey: enc(wrapped), IV: enc(iv), Ciphertext: enc(ciphertext), AAD: enc(aad), TS: ts}
}

func TestEncryptedPayloadBoundToUserAndTime(t *testing.T) {
	cs := testService(t)
	p := encryptedTestPayload(t, cs, "u")
	if _, err := cs.DecryptSensitiveDataForUser(p, "other"); err == nil {
		t.Fatal("foreign payload accepted")
	}
	if got, err := cs.DecryptSensitiveDataForUser(p, "u"); err != nil || got != "secret" {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := cs.DecryptSensitiveDataForUser(p, "u"); err == nil {
		t.Fatal("replayed encrypted payload accepted")
	}
	p.TS = 0
	if _, err := cs.DecryptSensitiveData(p); err == nil {
		t.Fatal("missing timestamp accepted")
	}
	p.TS = time.Now().Add(-10 * time.Minute).Unix()
	if _, err := cs.DecryptSensitiveData(p); err == nil {
		t.Fatal("expired payload accepted")
	}
	p = encryptedTestPayload(t, cs, "u")
	p.TS++
	if _, err := cs.DecryptSensitiveData(p); err == nil {
		t.Fatal("unauthenticated timestamp accepted")
	}
}

func TestCredentialStorageFailClosed(t *testing.T) {
	previous := globalCryptoService
	defer SetGlobalCryptoService(previous)
	SetGlobalCryptoService(nil)
	if _, err := (EncryptedString("private-key")).Value(); err == nil {
		t.Fatal("unencrypted secret write accepted")
	}
	var value EncryptedString
	if err := value.Scan("ENC:v1:invalid"); err == nil {
		t.Fatal("encrypted credential treated as plaintext")
	}
	cs := testService(t)
	SetGlobalCryptoService(cs)
	stored, err := (EncryptedString("private-key")).Value()
	if err != nil {
		t.Fatal(err)
	}
	if err := value.Scan(stored); err != nil || value != "private-key" {
		t.Fatalf("%s %v", value, err)
	}
	if err := value.Scan("ENC:v1:invalid"); err == nil {
		t.Fatal("corrupt encrypted credential accepted")
	}
	cs.dataKey = []byte("invalid")
	if _, err := (EncryptedString("private-key")).Value(); err == nil {
		t.Fatal("encryption failure wrote plaintext")
	}
}
