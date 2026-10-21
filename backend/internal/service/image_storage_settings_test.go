//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type stubSettingRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func newStubSettingRepo() *stubSettingRepo {
	return &stubSettingRepo{values: map[string]string{}}
}

func (r *stubSettingRepo) Get(context.Context, string) (*Setting, error) { return nil, nil }
func (r *stubSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.values[key], nil
}

func (r *stubSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}
func (r *stubSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *stubSettingRepo) SetMultiple(context.Context, map[string]string) error { return nil }
func (r *stubSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (r *stubSettingRepo) Delete(context.Context, string) error { return nil }

// reversibleEncryptor stands in for AES: prefixed so a test can tell ciphertext
// from plaintext, and so decrypting a plaintext value fails like the real one.
type reversibleEncryptor struct{}

func (reversibleEncryptor) Encrypt(plaintext string) (string, error) {
	return "enc:" + plaintext, nil
}

func (reversibleEncryptor) Decrypt(ciphertext string) (string, error) {
	rest, ok := strings.CutPrefix(ciphertext, "enc:")
	if !ok {
		return "", errors.New("not encrypted")
	}
	return rest, nil
}

type recordingStorage struct{ saved []string }

func (s *recordingStorage) Save(_ context.Context, key, _ string, _ []byte) (string, error) {
	s.saved = append(s.saved, key)
	return "https://cdn.example.com/" + key, nil
}

func newImageStorageFixture(t *testing.T, fallback config.ImageStorageConfig) (*ImageStorageSettingService, *stubSettingRepo, *[]config.ImageStorageConfig) {
	return newImageStorageFixtureWithKey(t, fallback, true)
}

func newImageStorageFixtureWithKey(t *testing.T, fallback config.ImageStorageConfig, encryptionKeyConfigured bool) (*ImageStorageSettingService, *stubSettingRepo, *[]config.ImageStorageConfig) {
	t.Helper()
	repo := newStubSettingRepo()
	encryptor := reversibleEncryptor{}
	backup := NewBackupService(repo, &config.Config{
		Totp: config.TotpConfig{EncryptionKeyConfigured: encryptionKeyConfigured},
	}, encryptor, nil, nil)

	var built []config.ImageStorageConfig
	factory := func(_ context.Context, cfg *config.ImageStorageConfig) (ImageStorage, error) {
		built = append(built, *cfg)
		return &recordingStorage{}, nil
	}
	return NewImageStorageSettingService(repo, encryptor, backup, factory, fallback), repo, &built
}

func seedBackupS3(t *testing.T, repo *stubSettingRepo, cfg BackupS3Config) {
	t.Helper()
	cfg.SecretAccessKey = "enc:" + cfg.SecretAccessKey
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, repo.Set(context.Background(), settingKeyBackupS3Config, string(data)))
}

// The admin switch must take effect without a restart: that is the entire point
// of moving image_storage out of config.yaml (#4542).
func TestImageStorageSettingsToggleTakesEffectWithoutRestart(t *testing.T) {
	svc, repo, built := newImageStorageFixture(t, config.ImageStorageConfig{})
	ctx := context.Background()
	seedBackupS3(t, repo, BackupS3Config{
		Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto",
		Bucket: "backup-bucket", AccessKeyID: "ak", SecretAccessKey: "sk",
		Prefix: "backups/",
	})

	uploader, enabled := svc.resolve()
	require.False(t, enabled, "disabled until an admin turns it on")
	require.Nil(t, uploader)

	_, err := svc.Update(ctx, ImageStorageSettings{Enabled: true, ReuseBackupS3: true})
	require.NoError(t, err)

	uploader, enabled = svc.resolve()
	require.True(t, enabled, "saving the setting must enable the feature immediately")
	require.NotNil(t, uploader)

	_, err = svc.Update(ctx, ImageStorageSettings{Enabled: false, ReuseBackupS3: true})
	require.NoError(t, err)
	_, enabled = svc.resolve()
	require.False(t, enabled, "turning it back off must also apply immediately")

	require.Len(t, *built, 1, "the S3 client is built only when the feature is on")
}

func TestImageStorageSettingsReuseBackupCredentials(t *testing.T) {
	svc, repo, built := newImageStorageFixture(t, config.ImageStorageConfig{})
	ctx := context.Background()
	seedBackupS3(t, repo, BackupS3Config{
		Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "wnam",
		Bucket: "backup-bucket", AccessKeyID: "backup-ak", SecretAccessKey: "backup-sk",
		Prefix: "backups/", ForcePathStyle: true,
	})

	_, err := svc.Update(ctx, ImageStorageSettings{Enabled: true, ReuseBackupS3: true, Prefix: "images"})
	require.NoError(t, err)
	_, enabled := svc.resolve()
	require.True(t, enabled)

	require.Len(t, *built, 1)
	got := (*built)[0]
	require.Equal(t, "https://acct.r2.cloudflarestorage.com", got.Endpoint)
	require.Equal(t, "wnam", got.Region)
	require.Equal(t, "backup-ak", got.AccessKeyID)
	require.Equal(t, "backup-sk", got.SecretAccessKey, "the backup secret must be decrypted before use")
	require.True(t, got.ForcePathStyle)
	require.Equal(t, "backup-bucket", got.Bucket, "an empty bucket falls back to the backup bucket")
	require.Equal(t, "images/", got.Prefix, "images stay under their own prefix so they never collide with backups/")

	// Reusing must not duplicate the secret into a second row.
	raw, err := repo.GetValue(ctx, settingKeyImageStorageConfig)
	require.NoError(t, err)
	require.NotContains(t, raw, "backup-sk")
	require.NotContains(t, raw, "enc:")
}

func TestImageStorageSettingsOwnCredentialsAreEncryptedAndMasked(t *testing.T) {
	svc, repo, built := newImageStorageFixture(t, config.ImageStorageConfig{})
	ctx := context.Background()

	saved, err := svc.Update(ctx, ImageStorageSettings{
		Enabled: true, Bucket: "my-images",
		Endpoint:    "https://acct.r2.cloudflarestorage.com",
		AccessKeyID: "ak", SecretAccessKey: "super-secret",
	})
	require.NoError(t, err)
	require.Empty(t, saved.SecretAccessKey, "the response must never echo the secret back")

	raw, err := repo.GetValue(ctx, settingKeyImageStorageConfig)
	require.NoError(t, err)
	require.NotContains(t, raw, `"secret_access_key":"super-secret"`, "the secret must be encrypted at rest")
	require.Contains(t, raw, "enc:super-secret")

	fetched, err := svc.Get(ctx)
	require.NoError(t, err)
	require.Empty(t, fetched.SecretAccessKey)
	require.True(t, svc.SecretConfigured(ctx))

	_, enabled := svc.resolve()
	require.True(t, enabled)
	require.Equal(t, "super-secret", (*built)[0].SecretAccessKey, "the stored secret must be decrypted before use")

	// An update that omits the secret keeps the stored one rather than wiping it.
	_, err = svc.Update(ctx, ImageStorageSettings{
		Enabled: true, Bucket: "my-images",
		Endpoint: "https://acct.r2.cloudflarestorage.com", AccessKeyID: "ak",
	})
	require.NoError(t, err)
	svc.resolve()
	require.Equal(t, "super-secret", (*built)[1].SecretAccessKey)
}

// Persisting the service's own S3 secret must be refused when the encryption key
// is auto-generated, otherwise the ciphertext cannot be decrypted after a
// restart (#4524). Reusing the backup credentials stays allowed because it does
// not persist a second copy of the secret.
func TestImageStorageSettingsRejectSecretWithEphemeralKey(t *testing.T) {
	svc, repo, built := newImageStorageFixtureWithKey(t, config.ImageStorageConfig{}, false)
	ctx := context.Background()

	_, err := svc.Update(ctx, ImageStorageSettings{
		Enabled: true, Bucket: "my-images",
		Endpoint:    "https://acct.r2.cloudflarestorage.com",
		AccessKeyID: "ak", SecretAccessKey: "super-secret",
	})
	require.ErrorIs(t, err, ErrSecretEncryptionKeyNotConfigured)

	raw, _ := repo.GetValue(ctx, settingKeyImageStorageConfig)
	require.Empty(t, raw, "nothing must be persisted when the secret is rejected")
	require.Empty(t, *built)

	// Reusing backup credentials does not persist a secret, so it stays allowed.
	seedBackupS3(t, repo, BackupS3Config{
		Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto",
		Bucket: "backup-bucket", AccessKeyID: "ak", SecretAccessKey: "sk", Prefix: "backups/",
	})
	_, err = svc.Update(ctx, ImageStorageSettings{Enabled: true, ReuseBackupS3: true})
	require.NoError(t, err)
}

func TestImageStorageSettingsIncompleteStaysDisabled(t *testing.T) {
	svc, _, built := newImageStorageFixture(t, config.ImageStorageConfig{})
	ctx := context.Background()

	_, err := svc.Update(ctx, ImageStorageSettings{Enabled: true, Bucket: "my-images"})
	require.NoError(t, err)

	_, enabled := svc.resolve()
	require.False(t, enabled, "missing credentials must not enable the feature")
	require.Empty(t, *built, "no client is built from an incomplete configuration")
}

// Deployments that already enabled the feature through config.yaml must keep
// working after the setting moves into the database.
func TestImageStorageSettingsFallBackToConfigFile(t *testing.T) {
	svc, _, built := newImageStorageFixture(t, config.ImageStorageConfig{
		Enabled: true, Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto",
		Bucket: "yaml-bucket", AccessKeyID: "yaml-ak", SecretAccessKey: "yaml-sk",
		Prefix: "images/", MaxDownloadByte: 1024,
	})

	_, enabled := svc.resolve()
	require.True(t, enabled, "config.yaml still enables the feature when nothing is stored yet")
	require.Equal(t, "yaml-bucket", (*built)[0].Bucket)

	fetched, err := svc.Get(context.Background())
	require.NoError(t, err)
	require.True(t, fetched.Enabled)
	require.Equal(t, "yaml-bucket", fetched.Bucket)
	require.Empty(t, fetched.SecretAccessKey)
}

func TestImageStorageURLModeIndependentOfEnabled(t *testing.T) {
	svc, _, built := newImageStorageFixture(t, config.ImageStorageConfig{})
	ctx := context.Background()

	require.Equal(t, ImageURLModePassthrough, svc.resolveURLPolicy().Mode, "从未保存过时默认 passthrough")

	_, err := svc.Update(ctx, ImageStorageSettings{Enabled: false, URLMode: "inline"})
	require.NoError(t, err)
	policy := svc.resolveURLPolicy()
	require.Equal(t, ImageURLModeInline, policy.Mode, "异步开关关闭时 inline 仍生效")
	require.Nil(t, policy.Storage)
	_, enabled := svc.resolve()
	require.False(t, enabled)
	require.Empty(t, *built, "inline 不建存储客户端")
}

func TestImageStorageURLModeFallsBackToConfigFile(t *testing.T) {
	svc, _, _ := newImageStorageFixture(t, config.ImageStorageConfig{URLMode: "INLINE"})
	require.Equal(t, ImageURLModeInline, svc.resolveURLPolicy().Mode)
	got, err := svc.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, ImageURLModeInline, got.URLMode)
}

func TestImageStorageURLModeEmptyKeepsStoredValueAndRejectsUnknown(t *testing.T) {
	svc, _, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
	ctx := context.Background()

	_, err := svc.Update(ctx, ImageStorageSettings{URLMode: "inline"})
	require.NoError(t, err)
	// 旧版前端整结构体保存时不带 url_mode。
	saved, err := svc.Update(ctx, ImageStorageSettings{Enabled: false})
	require.NoError(t, err)
	require.Equal(t, ImageURLModeInline, saved.URLMode)

	_, err = svc.Update(ctx, ImageStorageSettings{URLMode: "presigned"})
	require.ErrorIs(t, err, ErrInvalidImageURLMode)
	got, err := svc.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, ImageURLModeInline, got.URLMode, "非法值不落库")
}

func TestImageStorageURLModeRehostRequiresPublicBaseURL(t *testing.T) {
	svc, repo, built := newImageStorageFixture(t, config.ImageStorageConfig{})
	ctx := context.Background()
	seedBackupS3(t, repo, BackupS3Config{
		Endpoint: "https://acct.r2.cloudflarestorage.com", Region: "auto",
		Bucket: "backup-bucket", AccessKeyID: "ak", SecretAccessKey: "sk",
	})

	_, err := svc.Update(ctx, ImageStorageSettings{ReuseBackupS3: true, URLMode: "rehost"})
	require.NoError(t, err)
	policy := svc.resolveURLPolicy()
	require.Equal(t, ImageURLModeRehost, policy.Mode)
	require.Nil(t, policy.Storage, "presigned URL 带存储 endpoint 域名，未配 public_base_url 时退化为 inline")
	require.Empty(t, *built)

	_, err = svc.Update(ctx, ImageStorageSettings{ReuseBackupS3: true, URLMode: "rehost", PublicBaseURL: "https://img.example.com/", Prefix: "gen"})
	require.NoError(t, err)
	policy = svc.resolveURLPolicy()
	require.NotNil(t, policy.Storage, "rehost 只要求凭证齐全与 public_base_url，不要求开启异步")
	require.Equal(t, "gen/", policy.Prefix)
	require.Len(t, *built, 1)

	// TTL 到期重解析时配置未变，不重建客户端。
	svc.policyMu.Lock()
	svc.policyExpiresAt = time.Time{}
	svc.policyMu.Unlock()
	require.NotNil(t, svc.resolveURLPolicy().Storage)
	require.Len(t, *built, 1)
}

func TestImageStorageURLModeRehostWithoutCredentialsFallsBack(t *testing.T) {
	svc, _, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
	_, err := svc.Update(context.Background(), ImageStorageSettings{ReuseBackupS3: true, URLMode: "rehost", PublicBaseURL: "https://img.example.com"})
	require.NoError(t, err)
	policy := svc.resolveURLPolicy()
	require.Equal(t, ImageURLModeRehost, policy.Mode)
	require.Nil(t, policy.Storage, "复用的备份 S3 不存在时模式仍生效、存储为空（调用方按 inline 处理）")
}

func TestImageStorageURLModeTTLConvergesWithoutBroadcast(t *testing.T) {
	svc, repo, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
	now := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return now }
	require.Equal(t, ImageURLModePassthrough, svc.resolveURLPolicy().Mode)

	// 另一副本直接写库（本进程没有收到广播）。
	require.NoError(t, repo.Set(context.Background(), settingKeyImageStorageConfig, `{"url_mode":"inline"}`))
	require.Equal(t, ImageURLModePassthrough, svc.resolveURLPolicy().Mode, "TTL 内沿用缓存")
	now = now.Add(imageURLPolicyTTL + time.Second)
	require.Equal(t, ImageURLModeInline, svc.resolveURLPolicy().Mode, "TTL 到期后收敛")
}

func TestImageStorageUpdatePublishesChange(t *testing.T) {
	svc, _, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
	published := 0
	svc.SetChangePublisher(func(context.Context) { published++ })
	_, err := svc.Update(context.Background(), ImageStorageSettings{URLMode: "inline"})
	require.NoError(t, err)
	require.Equal(t, 1, published)
}

func TestImageStorageResolveRetriesAfterLoadFailure(t *testing.T) {
	svc, repo, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
	now := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return now }
	seedBackupS3(t, repo, BackupS3Config{Bucket: "backup-bucket", AccessKeyID: "ak", SecretAccessKey: "sk"})

	require.NoError(t, repo.Set(context.Background(), settingKeyImageStorageConfig, `{not json`))
	_, enabled := svc.resolve()
	require.False(t, enabled)

	// 设置恢复正常后，不必等保存或重启，重试间隔过后即生效。
	require.NoError(t, repo.Set(context.Background(), settingKeyImageStorageConfig, `{"enabled":true,"reuse_backup_s3":true}`))
	_, enabled = svc.resolve()
	require.False(t, enabled, "重试间隔内不反复查库")
	now = now.Add(imageStorageLoadRetry + time.Second)
	_, enabled = svc.resolve()
	require.True(t, enabled, "读设置失败不能被永久缓存为关闭")
}

func TestImageStoragePresignExpiryIsCapped(t *testing.T) {
	svc, _, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
	saved, err := svc.Update(context.Background(), ImageStorageSettings{PresignExpiry: 10_000})
	require.NoError(t, err)
	require.Equal(t, maxImagePresignExpiryHours, saved.PresignExpiry)
}

func TestImageStorageURLRewriteStrictSetting(t *testing.T) {
	svc, repo, _ := newImageStorageFixture(t, config.ImageStorageConfig{})
	ctx := context.Background()
	strictOn, strictOff := true, false

	got, err := svc.Get(ctx)
	require.NoError(t, err)
	require.NotNil(t, got.URLRewriteStrict)
	require.False(t, *got.URLRewriteStrict, "默认关闭：下载失败保留上游 url")
	require.False(t, svc.resolveURLPolicy().Strict)

	_, err = svc.Update(ctx, ImageStorageSettings{URLMode: "inline", URLRewriteStrict: &strictOn})
	require.NoError(t, err)
	policy := svc.resolveURLPolicy()
	require.True(t, policy.Strict, "保存后立即生效")

	// 旧版前端整结构体保存时不带该字段：沿用已保存的值。
	saved, err := svc.Update(ctx, ImageStorageSettings{URLMode: "inline"})
	require.NoError(t, err)
	require.True(t, *saved.URLRewriteStrict)
	require.True(t, svc.resolveURLPolicy().Strict)

	_, err = svc.Update(ctx, ImageStorageSettings{URLRewriteStrict: &strictOff})
	require.NoError(t, err)
	require.False(t, svc.resolveURLPolicy().Strict, "可随时关闭")

	// 升级前保存的旧数据没有这个字段：回落到 config.yaml。
	require.NoError(t, repo.Set(ctx, settingKeyImageStorageConfig, `{"url_mode":"inline"}`))
	svc.fallback.URLRewriteStrict = true
	svc.Invalidate()
	require.True(t, svc.resolveURLPolicy().Strict)
	got, err = svc.Get(ctx)
	require.NoError(t, err)
	require.True(t, *got.URLRewriteStrict)
}

func TestImageStorageURLRewriteStrictFromConfigFile(t *testing.T) {
	svc, _, _ := newImageStorageFixture(t, config.ImageStorageConfig{URLMode: "rehost", URLRewriteStrict: true})
	policy := svc.resolveURLPolicy()
	require.Equal(t, ImageURLModeRehost, policy.Mode)
	require.True(t, policy.Strict)
}
