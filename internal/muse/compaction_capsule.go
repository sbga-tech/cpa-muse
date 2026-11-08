package muse

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// These constants and the wire record port helps/antigravity_compaction.go.
// The fixed key preserves capsules across CPA restarts and credential failover;
// this is an interoperability envelope, not a secret-storage boundary.
const compactionCapsulePrefix = "cpa-ag-compact-v1:"

type compactionCapsuleData struct {
	Summary   string `json:"summary"`
	Model     string `json:"model"`
	CreatedAt int64  `json:"created_at"`
}

func compactionCipher() (cipher.AEAD, error) {
	key := sha256.Sum256([]byte("CLIProxyAPI"))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func sealCompaction(summary, model string) (string, error) {
	plaintext, err := json.Marshal(compactionCapsuleData{Summary: summary, Model: model, CreatedAt: time.Now().Unix()})
	if err != nil {
		return "", fmt.Errorf("marshal compaction capsule: %w", err)
	}
	gcm, err := compactionCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return compactionCapsulePrefix + base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

func unsealCompaction(capsule string) (string, error) {
	if !strings.HasPrefix(capsule, compactionCapsulePrefix) {
		return "", fmt.Errorf("invalid compaction capsule: unrecognized compaction capsule format")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(capsule, compactionCapsulePrefix))
	if err != nil {
		return "", fmt.Errorf("invalid compaction capsule: decode compaction capsule: %w", err)
	}
	gcm, err := compactionCipher()
	if err != nil {
		return "", err
	}
	if len(ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return "", fmt.Errorf("invalid compaction capsule: compaction capsule ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("invalid compaction capsule: invalid or corrupted compaction capsule: %w", err)
	}
	var data compactionCapsuleData
	if err := json.Unmarshal(plaintext, &data); err != nil {
		return "", fmt.Errorf("invalid compaction capsule: unmarshal compaction capsule: %w", err)
	}
	return data.Summary, nil
}
