package config

import (
	"os"
	"testing"
)

// 固定 server.shutdown_timeout 的三层取值优先级：默认值 < 配置文件 < 环境变量。
// 该 key 只靠 SetDefault + AutomaticEnv 生效（未显式 BindEnv），
// 若有人移除 setDefaults 中的注册，环境变量会静默失效，故用测试锁住。
func TestShutdownTimeoutPrecedence(t *testing.T) {
	const jwtLine = "jwt:\n  secret: shutdown-timeout-test-secret-0123456789abcdef\n"

	loadIn := func(t *testing.T, yaml string) *Config {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(dir+"/config.yaml", []byte(jwtLine+yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		old, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(old) })

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return cfg
	}

	t.Run("默认值", func(t *testing.T) {
		_ = os.Unsetenv("SERVER_SHUTDOWN_TIMEOUT")
		if got := loadIn(t, "").Server.ShutdownTimeout; got != 5 {
			t.Errorf("默认值 = %d, want 5", got)
		}
	})

	t.Run("配置文件覆盖默认值", func(t *testing.T) {
		_ = os.Unsetenv("SERVER_SHUTDOWN_TIMEOUT")
		if got := loadIn(t, "server:\n  shutdown_timeout: 42\n").Server.ShutdownTimeout; got != 42 {
			t.Errorf("配置文件值 = %d, want 42", got)
		}
	})

	t.Run("环境变量覆盖配置文件", func(t *testing.T) {
		t.Setenv("SERVER_SHUTDOWN_TIMEOUT", "300")
		if got := loadIn(t, "server:\n  shutdown_timeout: 42\n").Server.ShutdownTimeout; got != 300 {
			t.Errorf("环境变量未覆盖配置文件: got %d, want 300", got)
		}
	})

	t.Run("环境变量覆盖默认值", func(t *testing.T) {
		t.Setenv("SERVER_SHUTDOWN_TIMEOUT", "120")
		if got := loadIn(t, "").Server.ShutdownTimeout; got != 120 {
			t.Errorf("环境变量未覆盖默认值: got %d, want 120", got)
		}
	})
}
