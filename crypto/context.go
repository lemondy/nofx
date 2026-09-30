package crypto

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

func (cs *CryptoService) DecryptSensitiveDataForUser(payload *EncryptedPayload, userID string) (string, error) {
	if payload == nil || userID == "" {
		return "", errors.New("missing authenticated context")
	}
	aad, err := base64.RawURLEncoding.DecodeString(payload.AAD)
	if err != nil {
		return "", err
	}
	var data AADData
	if err := json.Unmarshal(aad, &data); err != nil {
		return "", err
	}
	if data.UserID != userID {
		return "", errors.New("encrypted payload belongs to another user")
	}
	plaintext, err := cs.DecryptSensitiveData(payload)
	if err != nil {
		return "", err
	}
	fingerprint := sha256.Sum256([]byte(payload.WrappedKey + "|" + payload.IV + "|" + payload.Ciphertext))
	key := hex.EncodeToString(fingerprint[:])
	cs.replayMu.Lock()
	defer cs.replayMu.Unlock()
	if cs.replay == nil {
		cs.replay = make(map[string]time.Time)
	}
	now := time.Now()
	if len(cs.replay) >= 20000 {
		for k, expiry := range cs.replay {
			if !now.Before(expiry) {
				delete(cs.replay, k)
			}
		}
	}
	if expiry, exists := cs.replay[key]; exists && now.Before(expiry) {
		return "", errors.New("encrypted payload already used")
	}
	if len(cs.replay) >= 20000 {
		return "", errors.New("encrypted request capacity exceeded")
	}
	cs.replay[key] = now.Add(6 * time.Minute)
	return plaintext, nil
}
