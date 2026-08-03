package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kardianos/service"
	"github.com/pkg/sftp"
	"github.com/robfig/cron/v3"
	"golang.org/x/crypto/ssh"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	actionPull = "pull"
	actionPush = "push"
)

type Config struct {
	SSHHost   string `json:"sshHost"`
	SSHPort   int    `json:"sshPort"`
	User      string `json:"user"`
	Password  string `json:"password"`
	LocalDir  string `json:"localDir"`
	RemoteDir string `json:"remoteDir"`
	Cron      string `json:"cron"`
	Action    string `json:"action"`
}

// Validate 檢查單筆同步設定是否合法。
//
// 這些錯誤全部應該在啟動時就被攔下：
//   - action 打錯，舊版要一路到 syncData、且在 SSH 都連上之後才會發現；
//   - cron 字串打錯，舊版是完全靜默 —— 該任務永遠不執行，日誌上毫無痕跡。
//
// 一次收集所有問題再回報，避免使用者改一個錯、重跑、再發現下一個。
func (c Config) Validate() error {
	var problems []string

	if strings.TrimSpace(c.SSHHost) == "" {
		problems = append(problems, "sshHost 不可為空")
	}
	if c.SSHPort < 1 || c.SSHPort > 65535 {
		problems = append(problems, fmt.Sprintf("sshPort %d 超出有效範圍 (1-65535)", c.SSHPort))
	}
	if strings.TrimSpace(c.User) == "" {
		problems = append(problems, "user 不可為空")
	}
	if strings.TrimSpace(c.LocalDir) == "" {
		problems = append(problems, "localDir 不可為空")
	}
	if strings.TrimSpace(c.RemoteDir) == "" {
		problems = append(problems, "remoteDir 不可為空")
	}
	if c.Action != actionPull && c.Action != actionPush {
		problems = append(problems, fmt.Sprintf("action %q 無效，必須是 %q 或 %q",
			c.Action, actionPull, actionPush))
	}

	// 用 cron.ParseStandard 驗證：這正是 cron.New() 預設採用的解析器，
	// 所以這裡通過就代表 AddFunc 也會通過。
	if strings.TrimSpace(c.Cron) == "" {
		problems = append(problems, "cron 不可為空")
	} else if _, err := cron.ParseStandard(c.Cron); err != nil {
		problems = append(problems, fmt.Sprintf("cron %q 無法解析: %v", c.Cron, err))
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

var configs []Config

// svcLogger 是 kardianos/service 提供的系統層 logger
// (Windows 事件檢視器 / Linux syslog)。檔案 log 尚未就緒、
// 或服務根本起不來時，只有這條管道送得出訊息。
var svcLogger service.Logger

// fileLogReady 標記 log 輸出是否已成功接到檔案。
// 在此之前 log 仍指向 stderr，避免把同一則訊息重複印到 stderr。
var fileLogReady bool

// startupFatalf 回報啟動期的致命錯誤後結束程式。
//
// 不能直接用 log.Fatalf：
//   - 服務模式下 log 已導向檔案，而服務起不來時往往連檔案都還沒開成，
//     SCM 只會顯示一個沒有內容的「服務啟動失敗」，完全無從查起；
//   - 互動模式下訊息會被寫進 log 檔，使用者的 console 一片空白。
//
// 因此這裡把訊息同時送往 console、log 檔與系統 logger。
func startupFatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, msg)
	if fileLogReady {
		log.Print(msg)
	}
	// 僅在服務模式下送系統 logger：互動模式時它同樣會寫 stderr，
	// 會讓使用者在 console 上看到重複的兩行。
	if svcLogger != nil && !service.Interactive() {
		_ = svcLogger.Error(msg)
	}
	os.Exit(1)
}

// consolef 同時輸出到 console 與 log 檔，
// 用於 install/uninstall/start/stop 這類由使用者從命令列觸發的子命令 ——
// 這些情境下只寫 log 檔等於沒有回饋。
func consolef(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Println(msg)
	if fileLogReady {
		log.Print(msg)
	}
}

type program struct {
	cron   *cron.Cron         // 將 cron 升級為結構屬性，方便在 Stop 中存取
	ctx    context.Context    // 用來向下傳遞取消訊號
	cancel context.CancelFunc // 用來觸發取消
	done   chan struct{}      // 用來等待主迴圈完全結束
}

func (p *program) Start(s service.Service) error {
	// 初始化 Context 與排程器
	p.ctx, p.cancel = context.WithCancel(context.Background())
	// SkipIfStillRunning：若上一輪同步尚未結束就到了下個排程點，直接跳過該次，
	// 避免兩批 goroutine 同時對同一組目錄讀寫。
	p.cron = cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger)))
	p.done = make(chan struct{})

	go p.run()
	return nil
}

func (p *program) Stop(s service.Service) error {
	log.Println("[INFO] 收到停止訊號，準備優雅關閉服務...")
	// 1. 觸發 Context 取消，通知底層的遞迴掃描停止派發新檔案
	p.cancel()

	// 2. 停止 cron 排程器 (不再觸發新時間點的排程)
	if p.cron != nil {
		cronCtx := p.cron.Stop()
		<-cronCtx.Done() // 阻塞等待：確保「已經觸發且正在執行中」的排程任務跑完
	}

	// 3. 等待主迴圈安全結束
	<-p.done
	log.Println("[INFO] 所有連線與傳輸已安全結束，服務正式關閉。")
	return nil
}

const (
	logDateLayout    = "2006-01-02"
	logFilePrefix    = "sync_service_"
	logRetentionDays = 30
)

func logFileName(dir, date string) string {
	return filepath.Join(dir, logFilePrefix+date+".log")
}

// dailyRotateWriter 在每次寫入時檢查日期是否已變更，
// 若跨過午夜則自動切換至新的日期檔名，同時保留 lumberjack 的大小切分能力。
type dailyRotateWriter struct {
	mu     sync.Mutex
	dir    string
	date   string
	logger *lumberjack.Logger
}

func newDailyRotateWriter(dir string) *dailyRotateWriter {
	today := time.Now().Format(logDateLayout)
	return &dailyRotateWriter{
		dir:  dir,
		date: today,
		logger: &lumberjack.Logger{
			Filename:   logFileName(dir, today),
			MaxSize:    10, // 每個日誌檔最大容量 (單位：MB)
			MaxBackups: 7,  // 同一天內最多保留幾個大小輪替檔
			MaxAge:     logRetentionDays,
			Compress:   true, // 壓縮舊日誌 (.gz)
		},
	}
}

func (w *dailyRotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()

	// 每次寫入前檢查日期，跨日時切換新檔名
	rotated := false
	if today := time.Now().Format(logDateLayout); today != w.date {
		w.date = today
		w.logger.Filename = logFileName(w.dir, today)
		_ = w.logger.Rotate() // 關閉舊檔，lumberjack 下次寫入時會建立新檔
		rotated = true
	}

	n, err := w.logger.Write(p)
	w.mu.Unlock()

	if rotated {
		// 必須在鎖外、且另開 goroutine 執行：cleanupOldLogs 內的 log 呼叫
		// 會再繞回本 Write，若持鎖同步呼叫會直接死結。
		go cleanupOldLogs(w.dir, logRetentionDays*24*time.Hour)
	}

	return n, err
}

// cleanupOldLogs 刪除超過保留期限的舊日期日誌。
//
// lumberjack 自身的 MaxAge/MaxBackups 只會清理「與當前 Filename 同前綴」的檔案，
// 而本 writer 的檔名含日期，一旦跨日，昨天以前的檔案就不再符合前綴，
// 永遠不會被 lumberjack 回收，磁碟會無限成長 —— 因此這裡自行清理。
func cleanupOldLogs(dir string, retain time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("[WARN] 無法讀取日誌目錄 %s: %v", dir, err)
		return
	}

	cutoff := time.Now().Add(-retain)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), logFilePrefix) {
			continue
		}
		// 以 mtime 判斷；使用中的當日檔案 mtime 必為新的，不會被誤刪。
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if err := os.Remove(full); err != nil {
			log.Printf("[WARN] 無法刪除過期日誌 %s: %v", full, err)
		}
	}
}

// setupLogger 將 log 輸出接到執行檔目錄下的 log 資料夾。
// 回傳錯誤而非直接 log.Fatal：此時 log 尚未就緒，
// 呼叫端才有辦法把失敗原因送到系統 logger 與 console。
func setupLogger(exeDir string) error {
	// 1. 定義 log 資料夾的路徑 (在執行檔所在目錄下的 "log" 資料夾)
	logDir := filepath.Join(exeDir, "log")

	// 2. 檢查並自動建立 log 資料夾
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("無法建立 log 資料夾 %s: %w", logDir, err)
	}

	// 3. 使用每日切分 writer 接管 log 輸出
	log.SetOutput(newDailyRotateWriter(logDir))

	// 設定日誌格式：加入日期與時間 (精確到微秒)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	fileLogReady = true

	// 4. 啟動時先清一次過期日誌 (服務若長期未跨日重啟，也不會累積)
	go cleanupOldLogs(logDir, logRetentionDays*24*time.Hour)
	return nil
}

func loadConfig(configPath string) error {
	file, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("unable to read config file: %w", err)
	}

	// 先解析到區域變數：驗證失敗時不應污染全域 configs
	var loaded []Config
	if err := json.Unmarshal(file, &loaded); err != nil {
		return fmt.Errorf("unable to parse config JSON: %w", err)
	}

	if len(loaded) == 0 {
		return errors.New("設定檔內沒有任何同步任務")
	}

	var problems []string
	for i, c := range loaded {
		if err := c.Validate(); err != nil {
			problems = append(problems, fmt.Sprintf("第 %d 筆 (remoteDir=%q): %v", i+1, c.RemoteDir, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("設定內容不合法:\n  - %s", strings.Join(problems, "\n  - "))
	}

	configs = loaded
	return nil
}

// selectConfigsByHost 篩選出 sshHost 相符的設定，host 為空時回傳全部。
//
// 主機名稱以不分大小寫比對 (DNS 名稱本來就不分大小寫)，
// 因此設定檔寫 "Example.com"、命令列打 "example.com" 也能對上。
func selectConfigsByHost(all []Config, host string) []Config {
	if host == "" {
		return all
	}

	var matched []Config
	for _, c := range all {
		if strings.EqualFold(strings.TrimSpace(c.SSHHost), strings.TrimSpace(host)) {
			matched = append(matched, c)
		}
	}
	return matched
}

// availableHosts 回傳設定檔中出現過的 sshHost 清單 (去重並保持原順序)，
// 供 -host 找不到相符設定時提示使用者。
func availableHosts(all []Config) []string {
	seen := make(map[string]bool)
	var hosts []string
	for _, c := range all {
		key := strings.ToLower(strings.TrimSpace(c.SSHHost))
		if seen[key] {
			continue
		}
		seen[key] = true
		hosts = append(hosts, c.SSHHost)
	}
	return hosts
}

func (p *program) run() {
	defer close(p.done) // 當 run 結束時，通知 Stop 可以放行了
	log.Println("[INFO] Configs read successfully")
	log.Println("[INFO] Starting sync service")

	// 註：Go 1.22 起 range 變數已是每輪獨立，無需再手動複製一份 cfg
	scheduled := 0
	for _, cfg := range configs {
		// AddFunc 的錯誤必須檢查：cron 字串有問題時，該任務會靜默地永遠不執行。
		// 設定已在載入時驗證過，正常情況不該走到這裡，但仍不能默默吞掉。
		if _, err := p.cron.AddFunc(cfg.Cron, func() {
			log.Println("[INFO] 排程啟動，準備同步資料夾: ", cfg.RemoteDir)
			// 將 p.ctx 往下傳遞
			syncFolder(p.ctx, cfg, "", "")
		}); err != nil {
			log.Printf("[ERROR] 排程註冊失敗，此任務不會執行 (cron=%q, remoteDir=%s): %v",
				cfg.Cron, cfg.RemoteDir, err)
			continue
		}
		scheduled++
	}

	if scheduled == 0 {
		log.Println("[ERROR] 沒有任何排程任務註冊成功，服務將閒置不做事")
	} else {
		log.Printf("[INFO] 已註冊 %d 個排程任務", scheduled)
	}

	p.cron.Start()
	// 阻塞在這裡，直到 Stop() 呼叫了 p.cancel()
	<-p.ctx.Done()
}

func createSSHConfig(user string, password string) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		// 沒有 Timeout 時，對方被防火牆黑洞掉會讓 Dial 無限期卡住，
		// 該排程任務等同永久停擺。
		Timeout: 30 * time.Second,
	}
}

func connectToSSHServer(host string, port int, config *ssh.ClientConfig) (*ssh.Client, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, err
	}

	// === 加入 SSH KeepAlive 機制 ===
	go func() {
		// 每 30 秒發送一次 KeepAlive
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			// 發送一個伺服器無法辨識但會安全忽略的請求
			_, _, err := conn.SendRequest("keepalive@golang.org", true, nil)
			if err != nil {
				// 如果發送失敗，通常代表連線已經中斷，退出 Goroutine
				log.Println("SSH KeepAlive failed, connection might be broken:", err)
				return
			}
		}
	}()
	// ===============================

	return conn, nil
}

func createNewClinet(conn *ssh.Client) (*sftp.Client, error) {
	client, err := sftp.NewClient(conn)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// generateDateSlice 產生 [startDate, endDate] 區間內的每一天。
//
// 回傳順序為「由新到舊」：同步優先處理最新的日期資料夾，
// 作業若在中途被中斷 (Ctrl+C、服務停止)，已完成的會是最近期、
// 通常也最重要的資料。
//
// startDate 晚於 endDate 時回傳空 slice。
func generateDateSlice(startDate, endDate string) ([]string, error) {
	start, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		return nil, err
	}
	end, err := time.Parse("2006-01-02", endDate)
	if err != nil {
		return nil, err
	}

	var dateSlice []string
	// 由 end 往回遞減到 start
	for !end.Before(start) {
		dateSlice = append(dateSlice, end.Format("2006-01-02"))
		end = end.AddDate(0, 0, -1)
	}
	return dateSlice, nil
}

const (
	// 同時進行的檔案傳輸數上限
	maxConcurrentTransfers = 10
	// 傳輸中的暫存檔副檔名；完成後才 rename 成正式檔名
	partSuffix = ".part"
)

// needsTransfer 判斷來源檔是否需要傳輸到目的地。
//
// dstErr 是「對目的地做 Stat 的錯誤」：
//   - 不存在 → 需要傳輸
//   - 其他錯誤 (權限不足、連線中斷…) → 回報錯誤，呼叫端必須跳過。
//     舊版在這裡會落到 dst.ModTime()，而 dst 為 nil，直接 panic 整個服務。
//
// 大小不同也視為需要傳輸：可涵蓋「內容變了但 mtime 沒變」，
// 也能自動修復先前被截斷、mtime 卻比來源新而永遠不會重傳的損毀檔。
func needsTransfer(src, dst os.FileInfo, dstErr error) (bool, error) {
	switch {
	case errors.Is(dstErr, fs.ErrNotExist):
		return true, nil
	case dstErr != nil:
		return false, dstErr
	case src.Size() != dst.Size():
		return true, nil
	default:
		return src.ModTime().After(dst.ModTime()), nil
	}
}

// acquire 取得傳輸名額；關閉服務時不會卡在已滿的號誌上。
func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func syncData(ctx context.Context, client *sftp.Client, localDir, remoteDir, action string) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentTransfers)
	// === 新增：原子計數器與時間紀錄 ===
	var successCount, failedCount uint64
	startTime := time.Now()
	log.Printf("[INFO] 開始同步作業 | 模式: %s | 來源/目的: %s <-> %s", action, localDir, remoteDir)
	// ===================================

	var err error
	if action == actionPull {
		err = pullData(ctx, client, localDir, remoteDir, &wg, sem, &successCount, &failedCount)
	} else if action == actionPush {
		err = pushData(ctx, client, localDir, remoteDir, &wg, sem, &successCount, &failedCount)
	} else {
		return fmt.Errorf("invalid action: %s", action)
	}

	wg.Wait() // 等待所有併發的檔案傳輸完成

	// === 新增：結束日誌與統計輸出 ===
	duration := time.Since(startTime)
	log.Printf("[INFO] 同步作業完成 | 模式: %s | 總耗時: %v", action, duration)
	log.Printf("[STAT] 本次同步統計 => 成功傳輸: %d 個檔案, 失敗: %d 個檔案",
		atomic.LoadUint64(&successCount),
		atomic.LoadUint64(&failedCount))
	// ===================================

	return err
}

// 函數簽名加入 successCount 與 failedCount 的指標
func pullData(ctx context.Context, client *sftp.Client, localDir, remoteDir string, wg *sync.WaitGroup, sem chan struct{}, successCount, failedCount *uint64) error {
	remoteFiles, err := client.ReadDir(remoteDir)
	if err != nil {
		return err
	}

	for _, file := range remoteFiles {
		// 檢查 Context 是否已經被取消
		if err := ctx.Err(); err != nil {
			log.Println("[INFO] 收到中斷訊號，立刻停止掃描與排隊後續檔案...")
			return err
		}

		remoteFilePath := path.Join(remoteDir, file.Name())
		localFilePath := filepath.Join(localDir, file.Name())

		if file.Mode()&os.ModeSymlink != 0 {
			log.Printf("[SKIP] 略過符號連結 (pull): %s", remoteFilePath)
			continue
		}

		// 不同步傳輸中的暫存檔
		if strings.HasSuffix(file.Name(), partSuffix) {
			continue
		}

		if file.IsDir() {
			if err := os.MkdirAll(localFilePath, os.ModePerm); err != nil {
				log.Printf("[ERROR] 無法建立本地目錄 %s: %v", localFilePath, err)
				continue
			}
			// 遞迴時把計數器指標往下傳
			if err := pullData(ctx, client, localFilePath, remoteFilePath, wg, sem, successCount, failedCount); err != nil {
				log.Printf("[ERROR] 無法讀取遠端目錄 %s: %v", remoteFilePath, err)
				continue
			}
		} else {
			// ReadDir 回傳的 file 本身就是帶 Size/ModTime 的 os.FileInfo，
			// 不需要再對每個檔案打一次 client.Stat —— 那等於每檔多一次 SFTP
			// round-trip，檔案數多又跨 WAN 時代價很可觀。
			// (符號連結已在上面跳過，因此這裡不存在 lstat/stat 語意差異)
			localFileInfo, statErr := os.Stat(localFilePath)
			transfer, err := needsTransfer(file, localFileInfo, statErr)
			if err != nil {
				log.Printf("[ERROR] 無法取得本地檔案資訊 %s: %v", localFilePath, err)
				atomic.AddUint64(failedCount, 1)
				continue
			}
			if !transfer {
				continue
			}

			// 先取號、再開 goroutine：號誌限制的必須是「實際存在的 goroutine 數」。
			// 舊版在 goroutine 內部才取號，大目錄會一次生出數十萬個阻塞中的
			// goroutine，記憶體直接爆掉。
			if err := acquire(ctx, sem); err != nil {
				log.Println("[INFO] 收到中斷訊號，停止派發後續檔案...")
				return err
			}

			wg.Add(1)
			go func(remote, local string, modTime time.Time) {
				defer wg.Done()
				defer func() { <-sem }()

				// 依據傳輸結果更新計數器
				if err := downloadFile(client, local, remote, modTime); err != nil {
					log.Printf("[FAIL] 檔案下載失敗 %s: %v", remote, err)
					atomic.AddUint64(failedCount, 1) // 失敗計數 +1
				} else {
					atomic.AddUint64(successCount, 1) // 成功計數 +1
				}
			}(remoteFilePath, localFilePath, file.ModTime())
		}
	}

	return nil
}

// 函數簽名加入 successCount 與 failedCount 的指標
func pushData(ctx context.Context, client *sftp.Client, localDir, remoteDir string, wg *sync.WaitGroup, sem chan struct{}, successCount, failedCount *uint64) error {
	localFiles, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}

	for _, file := range localFiles {
		// 檢查 Context 是否已經被取消
		if err := ctx.Err(); err != nil {
			log.Println("[INFO] 收到中斷訊號，立刻停止掃描與排隊後續檔案...")
			return err
		}

		localFilePath := filepath.Join(localDir, file.Name())
		remoteFilePath := path.Join(remoteDir, file.Name())

		if file.Type()&os.ModeSymlink != 0 {
			log.Printf("[SKIP] 略過符號連結 (push): %s", localFilePath)
			continue
		}

		// 不同步傳輸中的暫存檔 (含 pull 模式失敗後殘留的 .part)
		if strings.HasSuffix(file.Name(), partSuffix) {
			continue
		}

		if file.IsDir() {
			if err := client.MkdirAll(remoteFilePath); err != nil {
				log.Printf("[ERROR] 無法建立遠端目錄 %s: %v", remoteFilePath, err)
				continue
			}
			// 遞迴時把計數器指標往下傳
			if err := pushData(ctx, client, localFilePath, remoteFilePath, wg, sem, successCount, failedCount); err != nil {
				log.Printf("[ERROR] 無法讀取本地目錄 %s: %v", localFilePath, err)
				continue
			}
		} else {
			localFileInfo, err := os.Stat(localFilePath)
			if err != nil {
				log.Printf("[ERROR] 無法取得本地檔案資訊 %s: %v", localFilePath, err)
				continue
			}

			remoteFileInfo, statErr := client.Stat(remoteFilePath)
			transfer, err := needsTransfer(localFileInfo, remoteFileInfo, statErr)
			if err != nil {
				// 連線中斷時 client.Stat 回的是網路錯誤而非 ENOENT，
				// 舊版會在此對 nil 解引用而讓整個服務 panic。
				log.Printf("[ERROR] 無法取得遠端檔案資訊 %s: %v", remoteFilePath, err)
				atomic.AddUint64(failedCount, 1)
				continue
			}
			if !transfer {
				continue
			}

			// 先取號、再開 goroutine (原因同 pullData)
			if err := acquire(ctx, sem); err != nil {
				log.Println("[INFO] 收到中斷訊號，停止派發後續檔案...")
				return err
			}

			wg.Add(1)
			go func(local, remote string, modTime time.Time) {
				defer wg.Done()
				defer func() { <-sem }()

				// 依據傳輸結果更新計數器
				if err := uploadFile(client, local, remote, modTime); err != nil {
					log.Printf("[FAIL] 檔案上傳失敗 %s: %v", local, err)
					atomic.AddUint64(failedCount, 1) // 失敗計數 +1
				} else {
					atomic.AddUint64(successCount, 1) // 成功計數 +1
				}
			}(localFilePath, remoteFilePath, localFileInfo.ModTime())
		}
	}

	return nil
}

// downloadFile 先寫入 .part 暫存檔，完整寫完並 close 成功後才 rename 到目標路徑。
//
// 舊版直接以 os.Create 就地覆寫目標檔：一旦傳到一半失敗 (斷線、磁碟滿、服務被
// 強制結束)，本地會留下內容截斷、但 mtime 比遠端還新的檔案，之後每一輪比對都
// 判定「本地較新」而永久跳過 —— 一次失敗即造成靜默且不可恢復的資料損毀。
//
// 最後把 mtime 對齊遠端，讓後續比對是以「來源時間」而非「下載時間」為基準。
func downloadFile(client *sftp.Client, localFilePath, remoteFilePath string, remoteModTime time.Time) error {
	remoteFile, err := client.Open(remoteFilePath)
	if err != nil {
		return err
	}
	defer remoteFile.Close()

	tmpPath := localFilePath + partSuffix
	localFile, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	// 任何失敗路徑都清掉暫存檔，不留半成品。
	// 成功路徑已 rename，此處的 Remove 會是 no-op。
	defer func() {
		if localFile != nil {
			localFile.Close()
		}
		os.Remove(tmpPath)
	}()

	if _, err := remoteFile.WriteTo(localFile); err != nil {
		return err
	}
	// 必須顯式 Close 並檢查錯誤：緩衝資料的寫入失敗只會在這裡浮現
	if err := localFile.Close(); err != nil {
		return err
	}
	localFile = nil // 已關閉，避免 defer 重複 Close

	if err := os.Rename(tmpPath, localFilePath); err != nil {
		return err
	}

	if err := os.Chtimes(localFilePath, time.Now(), remoteModTime); err != nil {
		log.Printf("[WARN] 無法設定 %s 的修改時間: %v", localFilePath, err)
	}

	log.Println("Downloaded", remoteFilePath, "to", localFilePath)
	return nil
}

// uploadFile 與 downloadFile 對稱：先傳到遠端的 .part，成功後才 rename。
func uploadFile(client *sftp.Client, localFilePath, remoteFilePath string, localModTime time.Time) error {
	localFile, err := os.Open(localFilePath)
	if err != nil {
		return err
	}
	defer localFile.Close()

	tmpPath := remoteFilePath + partSuffix
	remoteFile, err := client.Create(tmpPath)
	if err != nil {
		return err
	}
	defer func() {
		if remoteFile != nil {
			remoteFile.Close()
		}
		client.Remove(tmpPath)
	}()

	// 用 sftp.File.ReadFrom 而非 localFile.WriteTo：前者才會走 sftp 的
	// 分塊併發寫入路徑，長距離連線的吞吐量差距明顯。
	if _, err := remoteFile.ReadFrom(localFile); err != nil {
		return err
	}
	if err := remoteFile.Close(); err != nil {
		return err
	}
	remoteFile = nil

	// PosixRename 可原子覆寫既有檔案；伺服器未支援該擴充時退回「先刪再改名」。
	if err := client.PosixRename(tmpPath, remoteFilePath); err != nil {
		client.Remove(remoteFilePath) // 不存在時的錯誤可忽略，成敗由下面的 Rename 決定
		if err := client.Rename(tmpPath, remoteFilePath); err != nil {
			return err
		}
	}

	if err := client.Chtimes(remoteFilePath, time.Now(), localModTime); err != nil {
		log.Printf("[WARN] 無法設定遠端 %s 的修改時間: %v", remoteFilePath, err)
	}

	log.Println("Uploaded", localFilePath, "to", remoteFilePath)
	return nil
}

func syncFolder(ctx context.Context, config Config, startDate, endDate string) {
	configSSH := createSSHConfig(config.User, config.Password)
	conn, err := connectToSSHServer(config.SSHHost, config.SSHPort, configSSH)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()
	client, err := createNewClinet(conn)
	if err != nil {
		log.Println(err)
		return
	}
	defer client.Close()

	if startDate != "" && endDate != "" {
		dates, err := generateDateSlice(startDate, endDate)
		if err != nil {
			log.Println("Failed to generate date slice:", err)
			return
		}

		for _, date := range dates {
			remoteDir := path.Join(config.RemoteDir, date)
			localDir := filepath.Join(config.LocalDir, date)
			action := config.Action
			log.Println("Syncing Date:", date)
			if err := syncData(ctx, client, localDir, remoteDir, action); err != nil {
				log.Println("Failed to sync folder:", err)
			}
		}
	} else {
		remoteDir := config.RemoteDir
		localDir := config.LocalDir
		action := config.Action
		if err := syncData(ctx, client, localDir, remoteDir, action); err != nil {
			log.Println("Failed to sync folder:", err)
		}
	}
}

func main() {
	svcConfig := &service.Config{
		Name:        "DataSyncService",
		DisplayName: "Data Sync Service",
		Description: "This service syncs data from remote server to local machine every 30 minutes",
	}

	prg := &program{}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		// 連服務實例都建不起來，此時尚無任何 logger 可用
		fmt.Fprintln(os.Stderr, "無法建立服務實例:", err)
		os.Exit(1)
	}

	// 盡早取得系統 logger：在檔案 log 就緒之前，
	// 這是服務模式下唯一能把錯誤送到事件檢視器 / syslog 的管道。
	if l, lerr := s.Logger(nil); lerr == nil {
		svcLogger = l
	}

	startDate := flag.String("startDate", "", "Start date for data sync (YYYY-MM-DD)")
	endDate := flag.String("endDate", "", "End date for data sync (YYYY-MM-DD)")
	host := flag.String("host", "", "只同步設定檔中 sshHost 相符的項目；未指定時同步全部")
	flag.Parse()

	// Load configuration at service start
	exePath, err := os.Executable()
	if err != nil {
		startupFatalf("無法取得執行檔路徑: %v", err)
	}
	exeDir := filepath.Dir(exePath)

	if err := setupLogger(exeDir); err != nil {
		startupFatalf("%v", err)
	}

	configPath := filepath.Join(exeDir, "configs.json")
	if err := loadConfig(configPath); err != nil {
		startupFatalf("無法載入設定檔 %s: %v", configPath, err)
	}

	// 子命令一律由使用者從命令列觸發，成敗都必須回饋到 console，
	// 不能只寫進 log 檔 (否則使用者執行完看不到任何輸出)。
	if len(os.Args) > 1 {
		serviceAction := os.Args[1]
		switch serviceAction {
		case "install":
			if err := s.Install(); err != nil {
				startupFatalf("安裝服務失敗: %v", err)
			}
			consolef("Service installed successfully")
			return
		case "uninstall":
			if err := s.Uninstall(); err != nil {
				startupFatalf("移除服務失敗: %v", err)
			}
			consolef("Service uninstalled successfully")
			return
		case "start":
			if err := s.Start(); err != nil {
				startupFatalf("啟動服務失敗: %v", err)
			}
			consolef("Service started successfully")
			return
		case "stop":
			if err := s.Stop(); err != nil {
				startupFatalf("停止服務失敗: %v", err)
			}
			consolef("Service stopped successfully")
			return
		}
	}

	// -host 只在手動同步模式下有作用。若沒帶日期範圍就會落到服務模式、
	// 使這個旗標被靜默忽略，因此明確擋下來。
	if *host != "" && (*startDate == "" || *endDate == "") {
		startupFatalf("-host 必須搭配 -startDate 與 -endDate 使用 (它只作用於手動同步模式)")
	}

	if *startDate != "" && *endDate != "" {
		// 依 -host 篩選要同步的設定
		targets := selectConfigsByHost(configs, *host)
		if len(targets) == 0 {
			startupFatalf("找不到 sshHost 為 %q 的設定；設定檔中可用的主機: %s",
				*host, strings.Join(availableHosts(configs), ", "))
		}

		if *host != "" {
			log.Printf("[INFO] 進入手動同步模式，僅同步 sshHost=%s (符合的設定共 %d 筆)",
				*host, len(targets))
		} else {
			log.Printf("[INFO] 進入手動同步模式，同步全部 %d 筆設定", len(targets))
		}

		// === 建立攔截 Ctrl+C 與系統終止訊號的 Context ===
		// NotifyContext 會在收到 os.Interrupt (Ctrl+C) 或 SIGTERM 時，自動觸發 ctx 的 Cancel
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop() // 確保程式結束時釋放監聽資源
		// ==============================================

		for _, config := range targets {
			log.Printf("[INFO] 開始手動同步資料夾: %s (%s)", config.RemoteDir, config.SSHHost)
			// 將這個帶有中斷保護的 ctx 傳遞進去
			syncFolder(ctx, config, *startDate, *endDate)
			// 檢查是否在中途被使用者按 Ctrl+C 中斷了
			if err := ctx.Err(); err != nil {
				log.Println("[WARN] 使用者已中斷手動同步作業！")
				break
			}
		}
		log.Println("[INFO] 手動同步作業結束")
		return
	}

	if err := s.Run(); err != nil {
		// 服務主迴圈異常結束：這則訊息必須進系統 logger，
		// 否則在 Windows 上完全查不到失敗原因。
		startupFatalf("服務執行失敗: %v", err)
	}
}
