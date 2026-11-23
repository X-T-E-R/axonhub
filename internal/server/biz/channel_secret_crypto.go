package biz

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/looplj/axonhub/internal/objects"
)

const (
	probeAPIKeyCipherPrefix = "enc:v1:"
	probeAPIKeyEnvVar       = "AXONHUB_PROBE_SECRET_KEY"
	encryptionKeyEnvVar     = "AXONHUB_ENCRYPTION_KEY"
	idRewriteSecretEnvVar   = "AXONHUB_ID_REWRITE_SECRET"
)

var (
	errProbeAPIKeyInvalidPlaceholder = errors.New("probeAPIKey cannot be blank or a masked placeholder")
	errProbeAPIKeyEncryptionNoKey    = errors.New("deployment probe encryption key (AXONHUB_PROBE_SECRET_KEY or AXONHUB_ENCRYPTION_KEY) is not configured")
	errIDRewriteSecretNotConfigured  = errors.New("deployment ID rewrite secret (AXONHUB_ID_REWRITE_SECRET or AXONHUB_ENCRYPTION_KEY) is not configured")
)

func ErrIDRewriteSecretMissing() error {
	return errIDRewriteSecretNotConfigured
}

func isMaskedSecretPlaceholder(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return true
	}
	if strings.Contains(trimmed, "***") || strings.Contains(trimmed, "•••") || strings.Contains(trimmed, "...") {
		return true
	}
	return false
}

// resolveDeploymentSecretMaterial derives domain-separated 32-byte key material
// strictly from deployment-level environment variables, NEVER from DB-stored
// system settings (such as the JWT secret readable via the systems GraphQL query).
func resolveDeploymentSecretMaterial(domain string, envVars []string, missingErr error) ([]byte, error) {
	for _, envName := range envVars {
		if raw := strings.TrimSpace(os.Getenv(envName)); raw != "" {
			sum := sha256.Sum256([]byte(domain + "\x00" + raw))
			return sum[:], nil
		}
	}
	return nil, missingErr
}

func probeAPIKeyEncryptionKey() ([]byte, error) {
	return resolveDeploymentSecretMaterial(
		"axonhub-probe-api-key-v1",
		[]string{probeAPIKeyEnvVar, encryptionKeyEnvVar},
		errProbeAPIKeyEncryptionNoKey,
	)
}

func probeSecretScopeAAD(preset objects.ChannelBalanceProbePreset) []byte {
	return []byte("axonhub:probe_api_key:v1:" + strings.TrimSpace(string(preset)))
}

// ResolveIDRewriteSecret resolves the deployment-level HMAC secret used by
// idRewriteRules strictly from environment configuration (never DB JWT secret,
// never exposed in GraphQL).
func ResolveIDRewriteSecret(_ context.Context, _ *SystemService) ([]byte, error) {
	return resolveDeploymentSecretMaterial(
		"axonhub-id-rewrite-hmac-v1",
		[]string{idRewriteSecretEnvVar, encryptionKeyEnvVar},
		errIDRewriteSecretNotConfigured,
	)
}

// IDRewriteSecret returns a deployment-level HMAC secret for hot-path idRewriteRules.
func (svc *ChannelService) IDRewriteSecret(ctx context.Context) ([]byte, error) {
	if raw := strings.TrimSpace(os.Getenv(idRewriteSecretEnvVar)); raw != "" {
		sum := sha256.Sum256([]byte("axonhub-id-rewrite-hmac-v1\x00" + raw))
		return sum[:], nil
	}
	if raw := strings.TrimSpace(os.Getenv(encryptionKeyEnvVar)); raw != "" {
		sum := sha256.Sum256([]byte("axonhub-id-rewrite-hmac-v1\x00" + raw))
		return sum[:], nil
	}
	if svc != nil {
		svc.idRewriteSecretMu.RLock()
		cached := svc.idRewriteSecret
		svc.idRewriteSecretMu.RUnlock()
		if len(cached) > 0 {
			return cached, nil
		}
	}
	return ResolveIDRewriteSecret(ctx, nil)
}

// EncryptProbeAPIKey encrypts a write-only probe API key using AES-256-GCM bound
// to the authorized preset scope via AEAD additional authenticated data (AAD).
func EncryptProbeAPIKey(_ context.Context, _ *SystemService, preset objects.ChannelBalanceProbePreset, plaintext string) (string, error) {
	trimmed := strings.TrimSpace(plaintext)
	if isMaskedSecretPlaceholder(trimmed) {
		return "", errProbeAPIKeyInvalidPlaceholder
	}
	key, err := probeAPIKeyEncryptionKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(trimmed), probeSecretScopeAAD(preset))
	return probeAPIKeyCipherPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptProbeAPIKey decrypts a preset-bound encrypted probe API key. Decryption
// fails cryptographically if the ciphertext is replayed under a different preset
// (e.g., an Exa Admin key cannot be decrypted after switching to a custom preset).
func DecryptProbeAPIKey(_ context.Context, _ *SystemService, preset objects.ChannelBalanceProbePreset, ciphertext string) (string, error) {
	trimmed := strings.TrimSpace(ciphertext)
	if trimmed == "" {
		return "", nil
	}
	if !strings.HasPrefix(trimmed, probeAPIKeyCipherPrefix) {
		return "", errors.New("invalid encrypted probe API key format")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(trimmed, probeAPIKeyCipherPrefix))
	if err != nil {
		return "", fmt.Errorf("decode probe API key ciphertext: %w", err)
	}
	key, err := probeAPIKeyEncryptionKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("corrupted probe API key ciphertext")
	}
	nonce, payload := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, payload, probeSecretScopeAAD(preset))
	if err != nil {
		return "", fmt.Errorf("decrypt probe API key: %w", err)
	}
	return string(plain), nil
}

// applyBalanceProbeSecretWrite enforces write-only PRESERVE / SET / CLEAR semantics
// on ChannelBalanceProbe.ProbeAPIKeyCiphertext, binds the secret to the preset scope,
// clears the secret if the preset changes without an explicit SET, and always clears
// transient plaintext input fields.
func (svc *ChannelService) applyBalanceProbeSecretWrite(ctx context.Context, current, next *objects.ChannelSettings) error {
	if next == nil || next.BalanceProbe == nil {
		return nil
	}
	var existingCipher string
	var existingPreset objects.ChannelBalanceProbePreset
	if current != nil && current.BalanceProbe != nil {
		existingCipher = strings.TrimSpace(current.BalanceProbe.ProbeAPIKeyCiphertext)
		existingPreset = current.BalanceProbe.Preset
	}

	probe := next.BalanceProbe
	op := probe.ProbeAPIKeyOp
	rawInput := probe.ProbeAPIKey
	// Always clear transient plaintext fields before returning.
	defer func() {
		probe.ProbeAPIKey = ""
		probe.ProbeAPIKeyOp = ""
	}()

	switch op {
	case "", objects.ChannelSecretWriteOpPreserve:
		if strings.TrimSpace(rawInput) != "" {
			return errors.New("probeAPIKeyOp must be set to 'set' when providing probeAPIKey")
		}
		// Generic edits may supply balanceProbe while omitting optional preset; preserve both.
		if probe.Preset == "" && existingPreset != "" {
			probe.Preset = existingPreset
		}
		// Never carry an admin probe key across an explicit preset change (e.g. Exa -> custom/other).
		if existingCipher != "" && probe.Preset != existingPreset {
			probe.ProbeAPIKeyCiphertext = ""
			return nil
		}
		probe.ProbeAPIKeyCiphertext = existingCipher
		return nil
	case objects.ChannelSecretWriteOpClear:
		probe.ProbeAPIKeyCiphertext = ""
		return nil
	case objects.ChannelSecretWriteOpSet:
		cipherText, err := EncryptProbeAPIKey(ctx, svc.SystemService, probe.Preset, rawInput)
		if err != nil {
			return err
		}
		probe.ProbeAPIKeyCiphertext = cipherText
		return nil
	default:
		return fmt.Errorf("unsupported probeAPIKeyOp %q", op)
	}
}
