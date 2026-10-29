package panel

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/protocol"
)

const recoveryPath = "/api/agent/startup-keys"

// RecoveryEnvelope uses a domain-separated authenticated envelope inside signed HTTPS requests.
type RecoveryEnvelope struct {
	IV         string `json:"iv"`
	Ciphertext string `json:"ciphertext"`
}

// RecoveryGrant binds ephemeral storage keys to one authenticated request.
type RecoveryGrant struct {
	Nonce      string `json:"nonce"`
	Locked     bool   `json:"locked"`
	Generation uint64 `json:"generation"`
	Keys       []struct {
		Dataset string `json:"dataset"`
		Key     string `json:"key"`
	} `json:"keys"`
}

func recoveryCipher(secret string) (cipher.AEAD, error) {
	key := sha256.Sum256([]byte("payincus-startup-v1\x00" + secret))
	block, err := aes.NewCipher(key[:])
	clear(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SealRecovery creates the interoperable AES-256-GCM recovery envelope.
func SealRecovery(secret, agentID, direction string, payload []byte) (RecoveryEnvelope, error) {
	aead, err := recoveryCipher(secret)
	if err != nil {
		return RecoveryEnvelope{}, err
	}
	iv := make([]byte, aead.NonceSize())
	_, err = rand.Read(iv)
	if err != nil {
		return RecoveryEnvelope{}, err
	}
	ciphertext := aead.Seal(nil, iv, payload, []byte("payincus-startup-v1:"+direction+":"+agentID))
	return RecoveryEnvelope{IV: base64.StdEncoding.EncodeToString(iv), Ciphertext: base64.StdEncoding.EncodeToString(ciphertext)}, nil
}

// OpenRecovery authenticates the direction and node identity before returning plaintext.
func OpenRecovery(secret, agentID, direction string, envelope RecoveryEnvelope) ([]byte, error) {
	aead, err := recoveryCipher(secret)
	if err != nil {
		return nil, err
	}
	iv, err := base64.StdEncoding.DecodeString(envelope.IV)
	if err != nil || len(iv) != aead.NonceSize() {
		return nil, fmt.Errorf("invalid envelope")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil || len(ciphertext) > 131072 {
		return nil, fmt.Errorf("invalid envelope")
	}
	return aead.Open(nil, iv, ciphertext, []byte("payincus-startup-v1:"+direction+":"+agentID))
}

// StartupKeys first authenticates the node, then proves freshness and token possession in a separate envelope.
func (client *Client) StartupKeys(ctx context.Context) (RecoveryGrant, error) {
	nonce, err := protocol.NewNonce()
	if err != nil {
		return RecoveryGrant{}, err
	}
	payload, err := json.Marshal(map[string]any{"token": client.agentSecret, "timestamp": time.Now().UnixMilli(), "nonce": nonce})
	if err != nil {
		return RecoveryGrant{}, err
	}
	defer clear(payload)
	envelope, err := SealRecovery(client.agentSecret, client.agentID, "request", payload)
	if err != nil {
		return RecoveryGrant{}, err
	}
	body, err := protocol.CanonicalJSON(envelope)
	if err != nil {
		return RecoveryGrant{}, err
	}
	timestamp := protocol.NewTimestamp()
	authNonce, err := protocol.NewNonce()
	if err != nil {
		return RecoveryGrant{}, err
	}
	hash := protocol.BodySHA256(body)
	req, err := http.NewRequestWithContext(ctx, "POST", client.panelURL+recoveryPath, bytes.NewReader(body))
	if err != nil {
		return RecoveryGrant{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-incudal-agent-id", client.agentID)
	req.Header.Set("x-incudal-timestamp", timestamp)
	req.Header.Set("x-incudal-nonce", authNonce)
	req.Header.Set("x-incudal-body-sha256", hash)
	req.Header.Set("x-incudal-signature", protocol.Signature(client.agentSecret, protocol.SigningPayload("POST", recoveryPath, timestamp, authNonce, hash)))
	response, err := client.httpClient.Do(req)
	if err != nil {
		return RecoveryGrant{}, fmt.Errorf("startup transport unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return RecoveryGrant{}, fmt.Errorf("startup service status %d", response.StatusCode)
	}
	var sealed RecoveryEnvelope
	err = json.NewDecoder(io.LimitReader(response.Body, 131072)).Decode(&sealed)
	if err != nil {
		return RecoveryGrant{}, fmt.Errorf("invalid startup response")
	}
	plain, err := OpenRecovery(client.agentSecret, client.agentID, "response", sealed)
	if err != nil {
		return RecoveryGrant{}, fmt.Errorf("startup response verification failed")
	}
	defer clear(plain)
	var grant RecoveryGrant
	err = json.Unmarshal(plain, &grant)
	if err != nil || grant.Nonce != nonce || len(grant.Keys) > 256 {
		return RecoveryGrant{}, fmt.Errorf("invalid startup grant")
	}
	return grant, nil
}
