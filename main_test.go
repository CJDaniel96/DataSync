package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// endlessReader 每次 Read 回傳 1 byte，用來模擬進行中的長時間傳輸。
// 首次被讀取時關閉 started，讓測試能在「確實開始搬資料之後」才取消，
// 不必輪詢共用欄位 (那本身就是 data race)。
type endlessReader struct {
	started chan struct{}
	once    sync.Once
}

func (s *endlessReader) Read(p []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	p[0] = 'x'
	return 1, nil
}

func TestCtxWriterStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var sink bytes.Buffer
	cw := &ctxWriter{ctx: ctx, w: &sink}

	if _, err := cw.Write([]byte("hello")); err != nil {
		t.Fatalf("取消前的寫入不應失敗: %v", err)
	}

	cancel()

	n, err := cw.Write([]byte("world"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("取消後應回傳 context.Canceled，實際為 %v", err)
	}
	if n != 0 {
		t.Errorf("取消後不應寫入任何位元組，實際寫入 %d", n)
	}
	if sink.String() != "hello" {
		t.Errorf("取消後的資料仍被寫出: %q", sink.String())
	}
}

func TestCtxReaderStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f, err := os.CreateTemp(t.TempDir(), "src")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("hello world"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cr := &ctxReader{ctx: ctx, f: f}

	buf := make([]byte, 5)
	if _, err := cr.Read(buf); err != nil {
		t.Fatalf("取消前的讀取不應失敗: %v", err)
	}

	cancel()

	if _, err := cr.Read(buf); !errors.Is(err, context.Canceled) {
		t.Errorf("取消後應回傳 context.Canceled，實際為 %v", err)
	}
}

// ctxReader 必須保留 Stat()，否則 sftp.File.ReadFrom 會判定來源大小未知
// 而退回單執行緒的循序寫入路徑，白白損失吞吐量。
func TestCtxReaderForwardsStat(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "src")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("0123456789"); err != nil {
		t.Fatal(err)
	}

	cr := &ctxReader{ctx: context.Background(), f: f}

	// 這正是 sftp.File.ReadFrom 用來取得大小的型別斷言
	sizer, ok := interface{}(cr).(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		t.Fatal("ctxReader 未實作 Stat()，ReadFrom 會退回循序路徑")
	}
	info, err := sizer.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 10 {
		t.Errorf("Stat().Size() = %d, 預期 10", info.Size())
	}
}

// 傳輸途中取消時，WriteTo/ReadFrom 應把 context 錯誤往上傳，
// 而不是靜默地寫出不完整的內容。
func TestCtxWriterPropagatesThroughCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var sink bytes.Buffer
	cw := &ctxWriter{ctx: ctx, w: &sink}

	// 複製到一半取消
	src := &endlessReader{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := io.CopyBuffer(cw, src, make([]byte, 1))
		done <- err
	}()

	<-src.started // 確定已開始搬資料後才取消
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("複製應以 context.Canceled 中止，實際為 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消後複製未在時限內停止")
	}
}

// keepAlive 必須在 context 取消時立刻結束。
//
// 舊版固定阻塞在 ticker 上，連線關閉後 goroutine 仍會殘留最多一個完整週期
// (30 秒)；每輪排程都建新連線的情況下會持續累積。
//
// 這裡 client 為 nil 也不會 panic —— ctx 已取消，select 會走 Done 分支，
// 根本不會碰到 client。
func TestKeepAliveExitsImmediatelyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &sshConn{ctx: ctx, cancel: cancel}

	done := make(chan struct{})
	go func() {
		c.keepAlive()
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("keepAlive 未在取消後立即結束 (KeepAlive 週期為 %v，代表仍卡在 ticker 上)", keepAliveInterval)
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

func TestSelectConfigsByHost(t *testing.T) {
	withHost := func(h, remote string) Config {
		c := validConfig()
		c.SSHHost = h
		c.RemoteDir = remote
		return c
	}
	all := []Config{
		withHost("alpha.example.com", "/a1"),
		withHost("beta.example.com", "/b1"),
		withHost("alpha.example.com", "/a2"),
	}

	t.Run("空字串回傳全部", func(t *testing.T) {
		if got := selectConfigsByHost(all, ""); len(got) != 3 {
			t.Errorf("預期 3 筆，實際 %d 筆", len(got))
		}
	})

	t.Run("挑出同一主機的多筆設定", func(t *testing.T) {
		got := selectConfigsByHost(all, "alpha.example.com")
		if len(got) != 2 {
			t.Fatalf("預期 2 筆，實際 %d 筆", len(got))
		}
		if got[0].RemoteDir != "/a1" || got[1].RemoteDir != "/a2" {
			t.Errorf("挑出的設定不正確: %+v", got)
		}
	})

	t.Run("主機名稱不分大小寫", func(t *testing.T) {
		if got := selectConfigsByHost(all, "ALPHA.Example.COM"); len(got) != 2 {
			t.Errorf("預期 2 筆，實際 %d 筆", len(got))
		}
	})

	t.Run("前後空白不影響比對", func(t *testing.T) {
		if got := selectConfigsByHost(all, "  beta.example.com  "); len(got) != 1 {
			t.Errorf("預期 1 筆，實際 %d 筆", len(got))
		}
	})

	t.Run("無相符主機回傳空", func(t *testing.T) {
		if got := selectConfigsByHost(all, "nope.example.com"); len(got) != 0 {
			t.Errorf("預期 0 筆，實際 %d 筆", len(got))
		}
	})

	t.Run("不做部分比對", func(t *testing.T) {
		if got := selectConfigsByHost(all, "alpha"); len(got) != 0 {
			t.Errorf("不應對主機名稱做前綴/子字串比對，實際挑出 %d 筆", len(got))
		}
	})
}

func TestAvailableHosts(t *testing.T) {
	withHost := func(h string) Config {
		c := validConfig()
		c.SSHHost = h
		return c
	}

	got := availableHosts([]Config{
		withHost("beta.example.com"),
		withHost("alpha.example.com"),
		withHost("BETA.example.com"), // 大小寫不同視為同一台
		withHost("alpha.example.com"),
	})

	want := []string{"beta.example.com", "alpha.example.com"}
	if len(got) != len(want) {
		t.Fatalf("預期 %d 台主機，實際 %d 台: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %s, 預期 %s (應去重並保持原順序)", i, got[i], want[i])
		}
	}
}

func TestGenerateDateSlice(t *testing.T) {
	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("長度 = %d, 預期 %d (%v)", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("[%d] = %s, 預期 %s", i, got[i], want[i])
			}
		}
	}

	t.Run("由新到舊排列", func(t *testing.T) {
		got, err := generateDateSlice("2026-08-01", "2026-08-03")
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, []string{"2026-08-03", "2026-08-02", "2026-08-01"})
	})

	t.Run("起訖同一天", func(t *testing.T) {
		got, err := generateDateSlice("2026-08-01", "2026-08-01")
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, []string{"2026-08-01"})
	})

	t.Run("跨月", func(t *testing.T) {
		got, err := generateDateSlice("2026-07-30", "2026-08-02")
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got, []string{"2026-08-02", "2026-08-01", "2026-07-31", "2026-07-30"})
	})

	t.Run("start 晚於 end 回傳空 slice", func(t *testing.T) {
		got, err := generateDateSlice("2026-08-05", "2026-08-01")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("預期空 slice，實際為 %v", got)
		}
	})

	t.Run("無效日期回傳錯誤", func(t *testing.T) {
		if _, err := generateDateSlice("not-a-date", "2026-08-03"); err == nil {
			t.Error("無效的 startDate 應回傳錯誤")
		}
		if _, err := generateDateSlice("2026-08-01", "nope"); err == nil {
			t.Error("無效的 endDate 應回傳錯誤")
		}
	})
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"context 取消", context.Canceled, false},
		{"context 逾時", context.DeadlineExceeded, false},
		{"檔案不存在", fs.ErrNotExist, false},
		{"包裝過的檔案不存在", &fs.PathError{Op: "open", Err: fs.ErrNotExist}, false},
		{"權限不足", fs.ErrPermission, false},
		{"一般網路錯誤視為暫時性", errors.New("connection reset by peer"), true},
		{"未知錯誤預設可重試", errors.New("boom"), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v, 預期 %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestTransferWithRetry(t *testing.T) {
	// 縮短退避時間，避免測試等待數秒
	origDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = origDelay })

	t.Run("第一次就成功則不重試", func(t *testing.T) {
		calls := 0
		err := transferWithRetry(context.Background(), "x", func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("非預期錯誤: %v", err)
		}
		if calls != 1 {
			t.Errorf("呼叫 %d 次，預期 1 次", calls)
		}
	})

	t.Run("暫時性失敗後成功", func(t *testing.T) {
		calls := 0
		err := transferWithRetry(context.Background(), "x", func() error {
			calls++
			if calls < 3 {
				return errors.New("connection reset")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("重試後應成功，實際為 %v", err)
		}
		if calls != 3 {
			t.Errorf("呼叫 %d 次，預期 3 次", calls)
		}
	})

	t.Run("持續失敗則在達到上限後放棄", func(t *testing.T) {
		calls := 0
		err := transferWithRetry(context.Background(), "x", func() error {
			calls++
			return errors.New("always fails")
		})
		if err == nil {
			t.Fatal("預期回傳錯誤")
		}
		if calls != maxTransferAttempts {
			t.Errorf("呼叫 %d 次，預期 %d 次", calls, maxTransferAttempts)
		}
	})

	t.Run("確定性錯誤不重試", func(t *testing.T) {
		calls := 0
		err := transferWithRetry(context.Background(), "x", func() error {
			calls++
			return fs.ErrNotExist
		})
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("應原樣回傳錯誤，實際為 %v", err)
		}
		if calls != 1 {
			t.Errorf("確定性錯誤不應重試，實際呼叫 %d 次", calls)
		}
	})

	t.Run("context 取消後不再重試", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		err := transferWithRetry(ctx, "x", func() error {
			calls++
			cancel() // 傳輸過程中被取消
			return errors.New("interrupted")
		})
		if err == nil {
			t.Fatal("預期回傳錯誤")
		}
		if calls != 1 {
			t.Errorf("取消後不應重試，實際呼叫 %d 次", calls)
		}
	})
}
