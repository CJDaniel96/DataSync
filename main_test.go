package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeInfo 只實作比對邏輯會用到的欄位
type fakeInfo struct {
	os.FileInfo
	size    int64
	modTime time.Time
}

func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) ModTime() time.Time { return f.modTime }

func TestNeedsTransfer(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	newer := base.Add(time.Hour)
	older := base.Add(-time.Hour)

	tests := []struct {
		name    string
		src     os.FileInfo
		dst     os.FileInfo
		dstErr  error
		want    bool
		wantErr bool
	}{
		{
			name:   "目的地不存在 → 需要傳輸",
			src:    fakeInfo{size: 100, modTime: base},
			dst:    nil,
			dstErr: fs.ErrNotExist,
			want:   true,
		},
		{
			name:   "目的地不存在(包裝過的錯誤) → 需要傳輸",
			src:    fakeInfo{size: 100, modTime: base},
			dst:    nil,
			dstErr: &fs.PathError{Op: "stat", Path: "x", Err: fs.ErrNotExist},
			want:   true,
		},
		{
			// 這是舊版直接 panic 的路徑：dst 為 nil 且錯誤不是 NotExist
			name:    "Stat 失敗(非不存在) → 回報錯誤且不可解引用 dst",
			src:     fakeInfo{size: 100, modTime: base},
			dst:     nil,
			dstErr:  fs.ErrPermission,
			want:    false,
			wantErr: true,
		},
		{
			name:   "來源較新 → 需要傳輸",
			src:    fakeInfo{size: 100, modTime: newer},
			dst:    fakeInfo{size: 100, modTime: base},
			dstErr: nil,
			want:   true,
		},
		{
			name:   "來源較舊 → 跳過",
			src:    fakeInfo{size: 100, modTime: older},
			dst:    fakeInfo{size: 100, modTime: base},
			dstErr: nil,
			want:   false,
		},
		{
			name:   "mtime 相同且大小相同 → 跳過(下載後 Chtimes 對齊的情境，需可重入)",
			src:    fakeInfo{size: 100, modTime: base},
			dst:    fakeInfo{size: 100, modTime: base},
			dstErr: nil,
			want:   false,
		},
		{
			// 舊版損毀檔的自我修復：內容被截斷、mtime 卻比來源新
			name:   "目的地較新但大小不同 → 仍需傳輸",
			src:    fakeInfo{size: 100, modTime: base},
			dst:    fakeInfo{size: 42, modTime: newer},
			dstErr: nil,
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := needsTransfer(tc.src, tc.dst, tc.dstErr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("預期回傳錯誤，實際為 nil")
				}
			} else if err != nil {
				t.Fatalf("非預期錯誤: %v", err)
			}
			if got != tc.want {
				t.Errorf("needsTransfer = %v, 預期 %v", got, tc.want)
			}
		})
	}
}

func TestCleanupOldLogs(t *testing.T) {
	dir := t.TempDir()

	now := time.Now()
	files := map[string]time.Time{
		"sync_service_2026-08-03.log":    now,                     // 當日，保留
		"sync_service_2026-07-20.log":    now.AddDate(0, 0, -14),  // 14 天前，保留
		"sync_service_2026-05-01.log":    now.AddDate(0, 0, -94),  // 94 天前，刪除
		"sync_service_2026-05-02.log.gz": now.AddDate(0, 0, -93),  // 壓縮檔也要刪
		"other_application.log":          now.AddDate(0, 0, -365), // 前綴不符，不可誤刪
	}
	for name, mt := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	cleanupOldLogs(dir, logRetentionDays*24*time.Hour)

	want := map[string]bool{
		"sync_service_2026-08-03.log":    true,
		"sync_service_2026-07-20.log":    true,
		"sync_service_2026-05-01.log":    false,
		"sync_service_2026-05-02.log.gz": false,
		"other_application.log":          true,
	}
	for name, shouldExist := range want {
		_, err := os.Stat(filepath.Join(dir, name))
		exists := !errors.Is(err, fs.ErrNotExist)
		if exists != shouldExist {
			t.Errorf("%s: 存在=%v, 預期存在=%v", name, exists, shouldExist)
		}
	}
}

func validConfig() Config {
	return Config{
		SSHHost:   "example.com",
		SSHPort:   22,
		User:      "u",
		Password:  "p",
		LocalDir:  "/local",
		RemoteDir: "/remote",
		Cron:      "0 * * * *",
		Action:    actionPull,
	}
}

func TestConfigValidate(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("合法設定不應回傳錯誤: %v", err)
	}

	// cron descriptor 也必須被接受 (cron.New 預設解析器支援)
	for _, expr := range []string{"@every 30m", "@daily", "*/15 * * * *"} {
		c := validConfig()
		c.Cron = expr
		if err := c.Validate(); err != nil {
			t.Errorf("cron %q 應為合法: %v", expr, err)
		}
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		// 錯誤訊息中應出現的關鍵字
		wantIn string
	}{
		{"空的 sshHost", func(c *Config) { c.SSHHost = "  " }, "sshHost"},
		{"port 為 0", func(c *Config) { c.SSHPort = 0 }, "sshPort"},
		{"port 超出範圍", func(c *Config) { c.SSHPort = 70000 }, "sshPort"},
		{"空的 user", func(c *Config) { c.User = "" }, "user"},
		{"空的 localDir", func(c *Config) { c.LocalDir = "" }, "localDir"},
		{"空的 remoteDir", func(c *Config) { c.RemoteDir = "" }, "remoteDir"},
		{"action 無效", func(c *Config) { c.Action = "sync" }, "action"},
		{"action 大小寫不符", func(c *Config) { c.Action = "Pull" }, "action"},
		{"空的 cron", func(c *Config) { c.Cron = "" }, "cron"},
		{"cron 欄位數不足", func(c *Config) { c.Cron = "0 *" }, "cron"},
		{"cron 內容無意義", func(c *Config) { c.Cron = "every hour" }, "cron"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("預期回傳錯誤，實際為 nil")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("錯誤訊息 %q 未包含 %q", err.Error(), tc.wantIn)
			}
		})
	}
}

func TestConfigValidateReportsAllProblems(t *testing.T) {
	c := validConfig()
	c.SSHHost = ""
	c.Action = "bogus"
	c.Cron = "nope"

	err := c.Validate()
	if err == nil {
		t.Fatal("預期回傳錯誤")
	}
	// 一次回報全部問題，而不是只講第一個
	for _, want := range []string{"sshHost", "action", "cron"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("錯誤訊息應同時包含 %q，實際為: %s", want, err.Error())
		}
	}
}

func TestLoadConfig(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "configs.json")
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("合法設定載入成功", func(t *testing.T) {
		configs = nil
		p := write(t, `[{"sshHost":"h","sshPort":22,"user":"u","password":"p",
			"localDir":"/l","remoteDir":"/r","cron":"@every 1h","action":"push"}]`)
		if err := loadConfig(p); err != nil {
			t.Fatalf("非預期錯誤: %v", err)
		}
		if len(configs) != 1 || configs[0].Action != actionPush {
			t.Errorf("configs 未正確載入: %+v", configs)
		}
	})

	t.Run("驗證失敗時不污染全域 configs", func(t *testing.T) {
		sentinel := []Config{validConfig()}
		configs = sentinel
		p := write(t, `[{"sshHost":"h","sshPort":22,"user":"u","localDir":"/l",
			"remoteDir":"/r","cron":"@every 1h","action":"bogus"}]`)
		err := loadConfig(p)
		if err == nil {
			t.Fatal("預期回傳錯誤")
		}
		if !strings.Contains(err.Error(), "action") {
			t.Errorf("錯誤訊息應指出 action 問題: %s", err.Error())
		}
		if len(configs) != 1 || configs[0].Action != actionPull {
			t.Errorf("全域 configs 不應被部分寫入: %+v", configs)
		}
	})

	t.Run("錯誤訊息標示是第幾筆", func(t *testing.T) {
		configs = nil
		p := write(t, `[
			{"sshHost":"h","sshPort":22,"user":"u","localDir":"/l","remoteDir":"/r1","cron":"@every 1h","action":"pull"},
			{"sshHost":"h","sshPort":22,"user":"u","localDir":"/l","remoteDir":"/r2","cron":"@every 1h","action":"bogus"}
		]`)
		err := loadConfig(p)
		if err == nil {
			t.Fatal("預期回傳錯誤")
		}
		if !strings.Contains(err.Error(), "第 2 筆") {
			t.Errorf("錯誤訊息應標示第 2 筆: %s", err.Error())
		}
	})

	t.Run("空陣列視為錯誤", func(t *testing.T) {
		configs = nil
		if err := loadConfig(write(t, `[]`)); err == nil {
			t.Error("空設定應回傳錯誤，否則服務會啟動後閒置不做事")
		}
	})

	t.Run("JSON 格式錯誤", func(t *testing.T) {
		configs = nil
		if err := loadConfig(write(t, `{not json`)); err == nil {
			t.Error("格式錯誤應回傳錯誤")
		}
	})

	t.Run("檔案不存在", func(t *testing.T) {
		if err := loadConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
			t.Error("檔案不存在應回傳錯誤")
		}
	})
}

func TestGenerateDateSlice(t *testing.T) {
	got, err := generateDateSlice("2026-08-01", "2026-08-03")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-08-01", "2026-08-02", "2026-08-03"}
	if len(got) != len(want) {
		t.Fatalf("長度 = %d, 預期 %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %s, 預期 %s", i, got[i], want[i])
		}
	}

	if _, err := generateDateSlice("not-a-date", "2026-08-03"); err == nil {
		t.Error("無效日期應回傳錯誤")
	}
}
