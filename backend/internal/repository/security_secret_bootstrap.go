package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/securitysecret"
	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	securitySecretKeyJWT = "jwt_secret"
	// securitySecretKeyTOTPKeyFingerprint 只存 TOTP_ENCRYPTION_KEY 的指纹（不可逆），
	// 用于在启动时发现多副本/多次部署之间密钥不一致。
	securitySecretKeyTOTPKeyFingerprint = "totp_encryption_key_fingerprint"
	totpKeyFingerprintDomain            = "sub2api:totp_encryption_key:v1:"
	securitySecretReadRetryMax          = 5
	securitySecretReadRetryWait         = 10 * time.Millisecond
)

var readRandomBytes = rand.Read

func ensureBootstrapSecrets(ctx context.Context, client *ent.Client, cfg *config.Config) error {
	if client == nil {
		return fmt.Errorf("nil ent client")
	}
	if cfg == nil {
		return fmt.Errorf("nil config")
	}

	if err := ensureJWTSecret(ctx, client, cfg); err != nil {
		return err
	}
	return ensureTOTPEncryptionKeyConsistent(ctx, client, cfg)
}

func ensureJWTSecret(ctx context.Context, client *ent.Client, cfg *config.Config) error {
	cfg.JWT.Secret = strings.TrimSpace(cfg.JWT.Secret)
	if cfg.JWT.Secret != "" {
		storedSecret, err := createSecuritySecretIfAbsent(ctx, client, securitySecretKeyJWT, cfg.JWT.Secret)
		if err != nil {
			return fmt.Errorf("persist jwt secret: %w", err)
		}
		if storedSecret != cfg.JWT.Secret {
			log.Println("Warning: configured JWT secret mismatches persisted value; using persisted secret for cross-instance consistency.")
		}
		cfg.JWT.Secret = storedSecret
		return nil
	}

	secret, created, err := getOrCreateGeneratedSecuritySecret(ctx, client, securitySecretKeyJWT, 32)
	if err != nil {
		return fmt.Errorf("ensure jwt secret: %w", err)
	}
	cfg.JWT.Secret = secret

	if created {
		log.Println("Warning: JWT secret auto-generated and persisted to database. Consider rotating to a managed secret for production.")
	}
	return nil
}

// ensureTOTPEncryptionKeyConsistent 断言所有共享同一数据库的实例使用同一把
// TOTP_ENCRYPTION_KEY。该密钥除 TOTP 外还加密插件配置、渠道监控 API key、
// 备份/图片存储 S3 secret、支付配置等；副本间不一致时一方写入的密文另一方
// 解不开，且不会报错直到读取。首次启动记录指纹，此后指纹不符即拒绝启动。
// 未显式配置（每进程随机生成）时相关功能本身被禁用，不做断言。
func ensureTOTPEncryptionKeyConsistent(ctx context.Context, client *ent.Client, cfg *config.Config) error {
	if !cfg.Totp.EncryptionKeyConfigured {
		return nil
	}
	key := strings.TrimSpace(cfg.Totp.EncryptionKey)
	if key == "" {
		return nil
	}
	fingerprint := totpEncryptionKeyFingerprint(key)
	stored, err := createSecuritySecretIfAbsent(ctx, client, securitySecretKeyTOTPKeyFingerprint, fingerprint)
	if err != nil {
		return fmt.Errorf("persist totp encryption key fingerprint: %w", err)
	}
	if stored != fingerprint {
		return fmt.Errorf("TOTP_ENCRYPTION_KEY does not match the key other instances sharing this database use "+
			"(fingerprint mismatch in security_secrets.%s); every replica must use the same key. "+
			"If the key was rotated intentionally and existing ciphertext has been re-encrypted, delete that row and restart",
			securitySecretKeyTOTPKeyFingerprint)
	}
	return nil
}

func totpEncryptionKeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(totpKeyFingerprintDomain + strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

func getOrCreateGeneratedSecuritySecret(ctx context.Context, client *ent.Client, key string, byteLength int) (string, bool, error) {
	existing, err := client.SecuritySecret.Query().Where(securitysecret.KeyEQ(key)).Only(ctx)
	if err == nil {
		value := strings.TrimSpace(existing.Value)
		if len([]byte(value)) < 32 {
			return "", false, fmt.Errorf("stored secret %q must be at least 32 bytes", key)
		}
		return value, false, nil
	}
	if !ent.IsNotFound(err) {
		return "", false, err
	}

	generated, err := generateHexSecret(byteLength)
	if err != nil {
		return "", false, err
	}

	if err := client.SecuritySecret.Create().
		SetKey(key).
		SetValue(generated).
		OnConflictColumns(securitysecret.FieldKey).
		DoNothing().
		Exec(ctx); err != nil {
		if !isSQLNoRowsError(err) {
			return "", false, err
		}
	}

	stored, err := querySecuritySecretWithRetry(ctx, client, key)
	if err != nil {
		return "", false, err
	}
	value := strings.TrimSpace(stored.Value)
	if len([]byte(value)) < 32 {
		return "", false, fmt.Errorf("stored secret %q must be at least 32 bytes", key)
	}
	return value, value == generated, nil
}

func createSecuritySecretIfAbsent(ctx context.Context, client *ent.Client, key, value string) (string, error) {
	value = strings.TrimSpace(value)
	if len([]byte(value)) < 32 {
		return "", fmt.Errorf("secret %q must be at least 32 bytes", key)
	}

	if err := client.SecuritySecret.Create().
		SetKey(key).
		SetValue(value).
		OnConflictColumns(securitysecret.FieldKey).
		DoNothing().
		Exec(ctx); err != nil {
		if !isSQLNoRowsError(err) {
			return "", err
		}
	}

	stored, err := querySecuritySecretWithRetry(ctx, client, key)
	if err != nil {
		return "", err
	}
	storedValue := strings.TrimSpace(stored.Value)
	if len([]byte(storedValue)) < 32 {
		return "", fmt.Errorf("stored secret %q must be at least 32 bytes", key)
	}
	return storedValue, nil
}

func querySecuritySecretWithRetry(ctx context.Context, client *ent.Client, key string) (*ent.SecuritySecret, error) {
	var lastErr error
	for attempt := 0; attempt <= securitySecretReadRetryMax; attempt++ {
		stored, err := client.SecuritySecret.Query().Where(securitysecret.KeyEQ(key)).Only(ctx)
		if err == nil {
			return stored, nil
		}
		if !isSecretNotFoundError(err) {
			return nil, err
		}
		lastErr = err
		if attempt == securitySecretReadRetryMax {
			break
		}

		timer := time.NewTimer(securitySecretReadRetryWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func isSecretNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	return ent.IsNotFound(err) || isSQLNoRowsError(err)
}

func isSQLNoRowsError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no rows in result set")
}

func generateHexSecret(byteLength int) (string, error) {
	if byteLength <= 0 {
		byteLength = 32
	}
	buf := make([]byte, byteLength)
	if _, err := readRandomBytes(buf); err != nil {
		return "", fmt.Errorf("generate random secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
