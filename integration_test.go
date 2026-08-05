package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// 掃描目錄樹，確認沒有殘留的 .part 暫存檔
func assertNoPartFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(p) == partSuffix {
			t.Errorf("殘留了暫存檔: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPullRoundTrip 驗證 pull 的基本正確性，含子目錄遞迴。
//
// 這同時涵蓋了先前移除 client.Stat 的改動: 現在 pullData 完全依賴
// ReadDir 回傳的 Size/ModTime 來做比對，若那些屬性不可靠，這個測試會失敗。
func TestPullRoundTrip(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()

	writeFile(t, filepath.Join(remote, "a.txt"), []byte("hello"))
	writeFile(t, filepath.Join(remote, "sub", "b.txt"), []byte("world"))
	writeFile(t, filepath.Join(remote, "sub", "deep", "c.bin"), bytes.Repeat([]byte{7}, 1<<16))

	if _, err := syncData(conn.ctx, client, local, remote, actionPull); err != nil {
		t.Fatalf("syncData 失敗: %v", err)
	}

	for _, tc := range []struct {
		rel  string
		want []byte
	}{
		{"a.txt", []byte("hello")},
		{filepath.Join("sub", "b.txt"), []byte("world")},
		{filepath.Join("sub", "deep", "c.bin"), bytes.Repeat([]byte{7}, 1<<16)},
	} {
		got := readFile(t, filepath.Join(local, tc.rel))
		if !bytes.Equal(got, tc.want) {
			t.Errorf("%s 內容不符 (長度 got=%d want=%d)", tc.rel, len(got), len(tc.want))
		}
	}

	assertNoPartFiles(t, local)
}

func TestPushRoundTrip(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()

	writeFile(t, filepath.Join(local, "a.txt"), []byte("hello"))
	writeFile(t, filepath.Join(local, "sub", "b.bin"), bytes.Repeat([]byte{3}, 1<<16))

	if _, err := syncData(conn.ctx, client, local, remote, actionPush); err != nil {
		t.Fatalf("syncData 失敗: %v", err)
	}

	if got := readFile(t, filepath.Join(remote, "a.txt")); !bytes.Equal(got, []byte("hello")) {
		t.Errorf("a.txt 內容不符: %q", got)
	}
	if got := readFile(t, filepath.Join(remote, "sub", "b.bin")); len(got) != 1<<16 {
		t.Errorf("b.bin 長度 = %d, 預期 %d", len(got), 1<<16)
	}

	assertNoPartFiles(t, remote)
}

// 傳輸後 mtime 會對齊來源，因此重跑應該完全跳過。
// 這裡把本地內容換成「同大小但不同內容」並保持 mtime，
// 若第二輪誤判為需要傳輸，內容會被覆寫回來而被偵測到。
func TestPullIsIdempotent(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	writeFile(t, filepath.Join(remote, "a.txt"), []byte("original"))

	if _, err := syncData(conn.ctx, client, local, remote, actionPull); err != nil {
		t.Fatalf("第一輪同步失敗: %v", err)
	}

	localFile := filepath.Join(local, "a.txt")
	info, err := os.Stat(localFile)
	if err != nil {
		t.Fatal(err)
	}

	// 同樣長度、不同內容；mtime 還原成同步後的值
	if err := os.WriteFile(localFile, []byte("MODIFIED"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localFile, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	if _, err := syncData(conn.ctx, client, local, remote, actionPull); err != nil {
		t.Fatalf("第二輪同步失敗: %v", err)
	}

	if got := readFile(t, localFile); !bytes.Equal(got, []byte("MODIFIED")) {
		t.Errorf("第二輪不應重新傳輸，但檔案被覆寫成 %q", got)
	}
}

// 大小不同就重傳 —— 這是舊版截斷檔的自我修復路徑。
// 本地檔內容不完整但 mtime 比遠端新，舊版的純 mtime 比對會永久跳過。
func TestPullRepairsTruncatedFile(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	content := []byte("the complete content")
	writeFile(t, filepath.Join(remote, "a.txt"), content)

	// 模擬先前傳輸中斷的結果: 內容截斷，但 mtime 是「現在」，比遠端新
	localFile := filepath.Join(local, "a.txt")
	writeFile(t, localFile, []byte("the comp"))
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(localFile, future, future); err != nil {
		t.Fatal(err)
	}

	if _, err := syncData(conn.ctx, client, local, remote, actionPull); err != nil {
		t.Fatalf("syncData 失敗: %v", err)
	}

	if got := readFile(t, localFile); !bytes.Equal(got, content) {
		t.Errorf("截斷檔未被修復，內容為 %q", got)
	}
}

// 取消後不得留下 .part 暫存檔或內容不完整的目標檔。
func TestDownloadFileCancelledLeavesNoResidue(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, _ := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	remoteFile := filepath.Join(remote, "big.bin")
	writeFile(t, remoteFile, bytes.Repeat([]byte{1}, 1<<20))

	localFile := filepath.Join(local, "big.bin")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消的 context: 確定性地走到取消路徑

	err := downloadFile(ctx, client, localFile, remoteFile, time.Now())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("預期 context.Canceled，實際為 %v", err)
	}
	if exists(localFile) {
		t.Error("取消後不應留下目標檔")
	}
	if exists(localFile + partSuffix) {
		t.Error("取消後不應留下 .part 暫存檔")
	}
}

// 傳輸「進行到一半」時取消: 等 .part 確實出現後才取消，
// 驗證中斷發生在傳輸途中而非開始之前。
func TestDownloadFileCancelledMidTransfer(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, _ := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	remoteFile := filepath.Join(remote, "big.bin")
	writeFile(t, remoteFile, bytes.Repeat([]byte{1}, 64<<20)) // 64 MB

	localFile := filepath.Join(local, "big.bin")
	partFile := localFile + partSuffix

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- downloadFile(ctx, client, localFile, remoteFile, time.Now())
	}()

	// 等到暫存檔確實開始長大，代表傳輸已在進行中
	deadline := time.Now().Add(10 * time.Second)
	for {
		if info, err := os.Stat(partFile); err == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Skip("傳輸在偵測到之前就完成了，無法測試中途取消")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("預期 context.Canceled，實際為 %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("取消後傳輸未在時限內停止")
	}

	if exists(localFile) {
		t.Error("中途取消不應留下目標檔")
	}
	if exists(partFile) {
		t.Error("中途取消不應留下 .part 暫存檔")
	}
}

// 傳輸失敗時，既有的本地檔案必須維持原狀。
//
// 這是 .part 暫存檔機制真正的價值所在: 舊版以 os.Create 就地覆寫，
// 一開檔就把既有內容截斷，中途失敗等於毀掉一份原本完好的資料。
func TestDownloadFailureLeavesExistingFileIntact(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, _ := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	remoteFile := filepath.Join(remote, "a.bin")
	writeFile(t, remoteFile, bytes.Repeat([]byte{1}, 1<<20))

	// 本地已有一份完整可用的舊檔
	localFile := filepath.Join(local, "a.bin")
	existing := []byte("EXISTING GOOD CONTENT")
	writeFile(t, localFile, existing)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := downloadFile(ctx, client, localFile, remoteFile, time.Now()); err == nil {
		t.Fatal("預期傳輸失敗")
	}

	got, err := os.ReadFile(localFile)
	if err != nil {
		t.Fatalf("既有檔案被刪除了: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Errorf("既有檔案遭破壞: 內容為 %q (長度 %d)，預期維持 %q", got, len(got), existing)
	}
	if exists(localFile + partSuffix) {
		t.Error("不應留下 .part 暫存檔")
	}
}

// 上傳方向的對應驗證: 取消後不得留下殘骸
func TestUploadFileCancelledLeavesNoResidue(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, _ := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	localFile := filepath.Join(local, "a.bin")
	writeFile(t, localFile, bytes.Repeat([]byte{1}, 1<<20))

	remoteFile := filepath.Join(remote, "a.bin")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := uploadFile(ctx, client, localFile, remoteFile, time.Now()); !errors.Is(err, context.Canceled) {
		t.Errorf("預期 context.Canceled，實際為 %v", err)
	}
	if exists(remoteFile) {
		t.Error("取消後不應留下目標檔")
	}
	if exists(remoteFile + partSuffix) {
		t.Error("取消後不應留下 .part 暫存檔")
	}
}

// 上傳失敗時，既有的遠端檔案必須維持原狀
func TestUploadFailureLeavesExistingFileIntact(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, _ := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	writeFile(t, filepath.Join(local, "a.bin"), bytes.Repeat([]byte{1}, 1<<20))

	remoteFile := filepath.Join(remote, "a.bin")
	existing := []byte("EXISTING GOOD CONTENT")
	writeFile(t, remoteFile, existing)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := uploadFile(ctx, client, filepath.Join(local, "a.bin"), remoteFile, time.Now()); err == nil {
		t.Fatal("預期傳輸失敗")
	}

	got, err := os.ReadFile(remoteFile)
	if err != nil {
		t.Fatalf("既有的遠端檔案被刪除了: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Errorf("既有的遠端檔案遭破壞: 內容為 %q，預期維持 %q", got, existing)
	}
}

// 符號連結兩個方向都應被略過
func TestSyncSkipsSymlinks(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()

	writeFile(t, filepath.Join(local, "real.txt"), []byte("real"))
	if err := os.Symlink(filepath.Join(local, "real.txt"), filepath.Join(local, "link.txt")); err != nil {
		t.Skipf("此環境無法建立符號連結: %v", err)
	}

	if _, err := syncData(conn.ctx, client, local, remote, actionPush); err != nil {
		t.Fatalf("syncData 失敗: %v", err)
	}

	if !exists(filepath.Join(remote, "real.txt")) {
		t.Error("一般檔案應被同步")
	}
	if exists(filepath.Join(remote, "link.txt")) {
		t.Error("符號連結不應被同步")
	}
}

// KeepAlive 偵測到連線中斷時，必須取消 conn.ctx 讓同步收手。
func TestKeepAliveCancelsContextOnBrokenConnection(t *testing.T) {
	// 縮短週期，否則要等 30 秒
	original := keepAliveInterval
	keepAliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = original })

	srv := newTestSFTPServer(t)
	_, conn := srv.dial(t)

	select {
	case <-conn.ctx.Done():
		t.Fatal("連線正常時 ctx 不應被取消")
	case <-time.After(100 * time.Millisecond):
		// 期望: 數個 KeepAlive 週期過去，連線仍健康
	}

	srv.breakConnections()

	select {
	case <-conn.ctx.Done():
		// KeepAlive 察覺連線已斷並取消了 ctx
	case <-time.After(5 * time.Second):
		t.Fatal("連線中斷後 KeepAlive 未取消 ctx，同步將繼續對死連線送請求")
	}
}

// 連線中斷後，進行中的同步應該停下來而不是繼續掃描
func TestSyncStopsWhenConnectionBreaks(t *testing.T) {
	original := keepAliveInterval
	keepAliveInterval = 20 * time.Millisecond
	t.Cleanup(func() { keepAliveInterval = original })

	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	for i := 0; i < 50; i++ {
		writeFile(t, filepath.Join(remote, string(rune('a'+i%26))+strconv.Itoa(i)+".bin"),
			bytes.Repeat([]byte{9}, 1<<16))
	}

	srv.breakConnections()

	done := make(chan error, 1)
	go func() { _, err := syncData(conn.ctx, client, local, remote, actionPull); done <- err }()

	select {
	case <-done:
		// 有錯誤或無錯誤都可接受，重點是它有結束而非卡住
	case <-time.After(30 * time.Second):
		t.Fatal("連線中斷後同步未結束")
	}
}

// syncData 必須回報統計數字，讓呼叫端能區分「完全成功」與「大部分失敗」。
func TestSyncDataReportsStats(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFile(t, filepath.Join(remote, n), []byte(n))
	}

	stats, err := syncData(conn.ctx, client, local, remote, actionPull)
	if err != nil {
		t.Fatalf("syncData 失敗: %v", err)
	}
	if stats.Succeeded != 3 || stats.Failed != 0 || stats.Skipped != 0 {
		t.Errorf("第一輪統計錯誤: %s", stats)
	}

	// 第二輪全部應被略過 —— 這正是舊版無從得知的資訊
	stats, err = syncData(conn.ctx, client, local, remote, actionPull)
	if err != nil {
		t.Fatalf("第二輪 syncData 失敗: %v", err)
	}
	if stats.Succeeded != 0 || stats.Failed != 0 || stats.Skipped != 3 {
		t.Errorf("第二輪統計錯誤，應全部略過: %s", stats)
	}
}

// 有檔案失敗時必須回傳錯誤。
// 舊版只回傳最上層掃描的錯誤，逐檔案失敗全被吞進日誌，呼叫端看到的是 nil。
func TestSyncDataReturnsErrorWhenFilesFail(t *testing.T) {
	// 縮短重試退避，否則失敗的檔案會讓這個測試等上數秒
	origDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = origDelay })

	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	writeFile(t, filepath.Join(remote, "ok.txt"), []byte("fine"))
	writeFile(t, filepath.Join(remote, "bad.txt"), []byte("will fail"))

	// 讓 bad.txt 的本地路徑先被佔成目錄，使 rename 必定失敗
	if err := os.MkdirAll(filepath.Join(local, "bad.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "bad.txt", "blocker"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	stats, err := syncData(conn.ctx, client, local, remote, actionPull)
	if err == nil {
		t.Fatalf("有檔案失敗時應回傳錯誤，實際為 nil (統計: %s)", stats)
	}
	if stats.Failed == 0 {
		t.Errorf("失敗計數應大於 0: %s", stats)
	}
	if stats.Succeeded == 0 {
		t.Errorf("成功的檔案仍應被計入: %s", stats)
	}
}

// 取消時回傳的錯誤要能被 errors.Is 辨識，
// 呼叫端才分得出「被中斷」與「真的失敗」。
func TestSyncDataCancellationIsIdentifiable(t *testing.T) {
	srv := newTestSFTPServer(t)
	client, _ := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	writeFile(t, filepath.Join(remote, "a.txt"), []byte("x"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := syncData(ctx, client, local, remote, actionPull)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("取消時的錯誤應可用 errors.Is 辨識，實際為 %v", err)
	}
}

// safeBuffer 是可同時被寫入與讀取的 log 目的地。
// log.Logger 本身有鎖，但測試在讀取內容時仍可能與殘留的背景 goroutine
// (例如上一個測試的 keepalive) 相撞，因此這裡自己再包一層。
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog 在測試期間把 log 導向緩衝區，結束後還原。
func captureLog(t *testing.T) *safeBuffer {
	t.Helper()
	original := log.Writer()
	buf := &safeBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(original) })
	return buf
}

// TestSyncFolderSyncDaysLimitsScanRange 驗證 syncDays 只會碰最近 N 天的資料夾。
//
// 這正是資料量成長時最關鍵的一項：舊資料夾連掃都不該掃到。
func TestSyncFolderSyncDaysLimitsScanRange(t *testing.T) {
	srv := newTestSFTPServer(t)

	remote := t.TempDir()
	local := t.TempDir()

	now := time.Now()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	old := now.AddDate(0, 0, -10).Format("2006-01-02")

	for _, date := range []string{today, yesterday, old} {
		writeFile(t, filepath.Join(remote, date, "data.txt"), []byte(date))
	}

	cfg := srv.config(t, local, remote, actionPull)
	cfg.SyncDays = 2
	syncFolder(t.Context(), cfg, "", "")

	for _, date := range []string{today, yesterday} {
		got := readFile(t, filepath.Join(local, date, "data.txt"))
		if string(got) != date {
			t.Errorf("%s 的內容 = %q, 預期 %q", date, got, date)
		}
	}
	if exists(filepath.Join(local, old)) {
		t.Errorf("%s 超出 syncDays 範圍，不該被同步", old)
	}
}

// syncDays 每小時跑一輪時，今天的資料夾在當天稍早本來就可能還沒產生。
// 這是正常狀態，不該每台主機每小時都刷一行假錯誤。
func TestSyncFolderSkipsMissingDateFolderQuietly(t *testing.T) {
	srv := newTestSFTPServer(t)

	remote := t.TempDir()
	local := t.TempDir()

	now := time.Now()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	// 只建立今天的，昨天的資料夾完全不存在
	writeFile(t, filepath.Join(remote, today, "data.txt"), []byte("ok"))

	logs := captureLog(t)
	cfg := srv.config(t, local, remote, actionPull)
	cfg.SyncDays = 2
	syncFolder(t.Context(), cfg, "", "")

	if got := readFile(t, filepath.Join(local, today, "data.txt")); string(got) != "ok" {
		t.Errorf("今天的資料未同步: %q", got)
	}

	out := logs.String()
	if !strings.Contains(out, "[SKIP]") || !strings.Contains(out, yesterday) {
		t.Errorf("缺少 %s 不存在的 [SKIP] 記錄，實際日誌:\n%s", yesterday, out)
	}
	if strings.Contains(out, "[ERROR]") {
		t.Errorf("資料夾尚未產生屬正常狀態，不該記成 ERROR，實際日誌:\n%s", out)
	}
}

// 真正的失敗仍必須記成 ERROR —— 上面的靜默化不能把問題一併吞掉。
func TestSyncFolderStillReportsRealFailures(t *testing.T) {
	srv := newTestSFTPServer(t)

	remote := t.TempDir()
	local := t.TempDir()
	today := time.Now().Format("2006-01-02")

	writeFile(t, filepath.Join(remote, today, "data.txt"), []byte("payload"))
	// 本地把同名路徑佔成檔案，pull 時建立目錄必定失敗
	writeFile(t, filepath.Join(local, today, "sub"), []byte("blocker"))
	writeFile(t, filepath.Join(remote, today, "sub", "inner.txt"), []byte("x"))

	logs := captureLog(t)
	cfg := srv.config(t, local, remote, actionPull)
	cfg.SyncDays = 1
	syncFolder(t.Context(), cfg, "", "")

	if out := logs.String(); !strings.Contains(out, "[ERROR]") {
		t.Errorf("真正的失敗應記成 ERROR，實際日誌:\n%s", out)
	}
}

// 手動指定的日期必須蓋過設定檔的 syncDays
func TestSyncFolderManualDatesOverrideSyncDays(t *testing.T) {
	srv := newTestSFTPServer(t)

	remote := t.TempDir()
	local := t.TempDir()

	today := time.Now().Format("2006-01-02")
	writeFile(t, filepath.Join(remote, today, "recent.txt"), []byte("recent"))
	writeFile(t, filepath.Join(remote, "2020-01-02", "old.txt"), []byte("old"))

	cfg := srv.config(t, local, remote, actionPull)
	cfg.SyncDays = 1
	syncFolder(t.Context(), cfg, "2020-01-02", "2020-01-02")

	if got := readFile(t, filepath.Join(local, "2020-01-02", "old.txt")); string(got) != "old" {
		t.Errorf("指定日期的資料未同步: %q", got)
	}
	if exists(filepath.Join(local, today)) {
		t.Error("手動模式下不該改用 syncDays 的範圍")
	}
}

// 全域號誌壓到 1 時，傳輸仍必須全部完成，且名額不能有殘留 ——
// 少還一個名額就會讓後續所有同步任務永久少一個併發額度。
func TestGlobalTransferCapDoesNotLeakSlots(t *testing.T) {
	withGlobalTransferSem(t, 1)

	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()

	const fileCount = 20
	for i := 0; i < fileCount; i++ {
		writeFile(t, filepath.Join(remote, "f"+strconv.Itoa(i)+".txt"), []byte(strconv.Itoa(i)))
	}

	stats, err := syncData(conn.ctx, client, local, remote, actionPull)
	if err != nil {
		t.Fatalf("syncData 失敗: %v", err)
	}
	if stats.Succeeded != fileCount {
		t.Errorf("成功數 = %d, 預期 %d", stats.Succeeded, fileCount)
	}
	if n := len(globalTransferSem); n != 0 {
		t.Errorf("全域號誌殘留 %d 個名額，應已全部歸還", n)
	}
}

// push 方向的日期資料夾同樣要能自己建立遠端目錄
func TestSyncFolderSyncDaysPush(t *testing.T) {
	srv := newTestSFTPServer(t)

	remote := t.TempDir()
	local := t.TempDir()

	now := time.Now()
	today := now.Format("2006-01-02")
	old := now.AddDate(0, 0, -10).Format("2006-01-02")

	writeFile(t, filepath.Join(local, today, "data.txt"), []byte("today"))
	writeFile(t, filepath.Join(local, old, "data.txt"), []byte("old"))

	cfg := srv.config(t, local, remote, actionPush)
	cfg.SyncDays = 1
	syncFolder(t.Context(), cfg, "", "")

	if got := readFile(t, filepath.Join(remote, today, "data.txt")); string(got) != "today" {
		t.Errorf("今天的資料未上傳: %q", got)
	}
	if exists(filepath.Join(remote, old)) {
		t.Errorf("%s 超出 syncDays 範圍，不該被同步", old)
	}
}
