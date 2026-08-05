package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
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
	// SyncDays > 0 時，排程模式只同步最近 N 天的日期子資料夾
	// (localDir/<日期> ↔ remoteDir/<日期>)，含今天往回數 N 天。
	// 0 (預設) 表示同步整個 localDir ↔ remoteDir。
	SyncDays int `json:"syncDays"`
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
	if c.SyncDays < 0 {
		problems = append(problems, fmt.Sprintf("syncDays %d 不可為負數 (0 表示同步整個目錄)", c.SyncDays))
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

// countConfigsWithSyncDays 回傳有設定 syncDays 的筆數。
func countConfigsWithSyncDays(all []Config) int {
	n := 0
	for _, c := range all {
		if c.SyncDays > 0 {
			n++
		}
	}
	return n
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

// maxStaggerDelay 是排程錯開的最大幅度。
const maxStaggerDelay = 5 * time.Minute

// staggerKey 是這筆設定在錯開計算中的識別字串。
// 同一台主機的不同目錄也要各自錯開，因此三個欄位都納入。
func (c Config) staggerKey() string {
	return c.SSHHost + "|" + c.RemoteDir + "|" + c.LocalDir
}

// scheduleInterval 以連續兩個觸發點推算排程間隔；無法判斷時回傳 0。
func scheduleInterval(sched cron.Schedule, now time.Time) time.Duration {
	first := sched.Next(now)
	if first.IsZero() {
		return 0
	}
	second := sched.Next(first)
	if second.IsZero() {
		return 0
	}
	return second.Sub(first)
}

// staggerDelay 依設定內容算出一個固定的起跑延遲，把撞在同一個時間點的任務攤開。
//
// 二十幾台主機的 cron 若都寫 `0 * * * *`，整點會同時開出全部連線，
// 瞬間把頻寬與磁碟 I/O 打滿，結果是每一台都變慢。
//
// 用雜湊而非亂數：同一筆設定每次啟動都拿到相同的延遲，日誌時間才有可預期性，
// 排查問題時也不會每次重啟都換一個時間點。
func staggerDelay(key string, interval time.Duration) time.Duration {
	window := maxStaggerDelay
	// 延遲不能吃掉整個排程間隔：@every 30s 這種高頻排程若延遲 5 分鐘，
	// 每一輪都會被 SkipIfStillRunning 擋掉而完全不執行。取間隔的一半為上限。
	if interval > 0 && interval/2 < window {
		window = interval / 2
	}
	if window <= 0 {
		return 0
	}

	// 必須用 64 位元雜湊：fnv32 的最大值約 4.3e9，換算成 ns 只有 4.3 秒，
	// 遠小於 5 分鐘的視窗，取餘數後所有任務都會擠在最前面的幾秒內。
	h := fnv.New64a()
	_, _ = h.Write([]byte(key)) // hash.Hash 的 Write 不會回傳錯誤
	return time.Duration(h.Sum64() % uint64(window))
}

// sleepCtx 等待 d，期間 ctx 被取消就提前結束。回傳是否等滿。
//
// 不用 time.Sleep：服務關閉時不該為了一個還沒開始的排程再等上幾分鐘。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *program) run() {
	defer close(p.done) // 當 run 結束時，通知 Stop 可以放行了
	log.Println("[INFO] Configs read successfully")
	log.Println("[INFO] Starting sync service")

	// 註：Go 1.22 起 range 變數已是每輪獨立，無需再手動複製一份 cfg
	scheduled := 0
	now := time.Now()
	for _, cfg := range configs {
		// 排程字串在載入設定時已驗證過，正常情況不該解析失敗，但仍不能默默吞掉 ——
		// 舊版用 AddFunc 且不檢查錯誤時，出問題的任務會永遠不執行且日誌毫無痕跡。
		sched, err := cron.ParseStandard(cfg.Cron)
		if err != nil {
			log.Printf("[ERROR] 排程註冊失敗，此任務不會執行 (cron=%q, remoteDir=%s): %v",
				cfg.Cron, cfg.RemoteDir, err)
			continue
		}

		delay := staggerDelay(cfg.staggerKey(), scheduleInterval(sched, now))
		p.cron.Schedule(sched, cron.FuncJob(func() {
			// 錯開起跑時間，避免所有任務在同一個時間點一起衝
			if !sleepCtx(p.ctx, delay) {
				return
			}
			log.Println("[INFO] 排程啟動，準備同步資料夾: ", cfg.RemoteDir)
			// 將 p.ctx 往下傳遞
			syncFolder(p.ctx, cfg, "", "")
		}))
		if delay > 0 {
			log.Printf("[INFO] 已註冊排程 %s (cron=%q, 起跑延遲 %v)", cfg.RemoteDir, cfg.Cron, delay)
		} else {
			log.Printf("[INFO] 已註冊排程 %s (cron=%q)", cfg.RemoteDir, cfg.Cron)
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

// keepAliveInterval 為 KeepAlive 的發送週期。
// 宣告為 var 而非 const，是為了讓整合測試能縮短週期以驗證連線中斷的處理。
var keepAliveInterval = 30 * time.Second

// sshConn 綁定一條 SSH 連線與它的存活狀態。
//
// ctx 衍生自呼叫端傳入的 context，並在以下情形額外被取消:
//   - KeepAlive 偵測到連線已中斷
//   - Close() 被呼叫
//
// 同步流程一律使用這個 ctx，連線一斷就能立刻收手，
// 而不是繼續對一條死掉的連線送請求、直到每個操作各自逾時。
type sshConn struct {
	client *ssh.Client
	ctx    context.Context
	cancel context.CancelFunc
}

func (c *sshConn) Close() error {
	c.cancel() // 同時讓 keepAlive goroutine 立即結束，不必等下一個 tick
	return c.client.Close()
}

// keepAlive 定期送出 keepalive 請求；一旦失敗就取消 ctx。
//
// 舊版只印一行 log 就結束 goroutine，正在進行的同步完全不知情；
// 且因為固定阻塞在 ticker 上，連線正常關閉後仍會殘留最多一個週期。
func (c *sshConn) keepAlive() {
	t := time.NewTicker(keepAliveInterval)
	defer t.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return // 連線已關閉或同步已取消
		case <-t.C:
			// 送出一個伺服器無法辨識、但會安全忽略的請求
			if _, _, err := c.client.SendRequest("keepalive@golang.org", true, nil); err != nil {
				log.Printf("[ERROR] SSH KeepAlive 失敗，連線可能已中斷，中止本次同步: %v", err)
				c.cancel()
				return
			}
		}
	}
}

func connectToSSHServer(ctx context.Context, host string, port int, config *ssh.ClientConfig) (*sshConn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, err
	}

	connCtx, cancel := context.WithCancel(ctx)
	c := &sshConn{client: client, ctx: connCtx, cancel: cancel}
	go c.keepAlive()

	return c, nil
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

// recentDateSlice 產生「今天往回數 days 天」的日期字串，由新到舊。
//
// days=1 只有今天，days=7 是今天加前面 6 天，與 generateDateSlice 一樣含頭尾。
// days<=0 回傳 nil，代表不套用日期資料夾、同步整個目錄。
//
// 用 AddDate 逐日回推而非減去 24h：後者遇到日光節約時間的日子會算出前一天
// 或同一天，產生重複或缺漏的資料夾名稱。
func recentDateSlice(now time.Time, days int) []string {
	if days <= 0 {
		return nil
	}
	dates := make([]string, 0, days)
	for i := 0; i < days; i++ {
		dates = append(dates, now.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return dates
}

const (
	// 單一同步任務同時進行的檔案傳輸數上限
	maxConcurrentTransfers = 10
	// 全部同步任務加總的傳輸數上限，見 globalTransferSem
	maxGlobalTransfers = 32
	// 傳輸中的暫存檔副檔名；完成後才 rename 成正式檔名
	partSuffix = ".part"
	// 同步過程建立本地目錄時的權限。
	//
	// 舊版用 os.ModePerm (0777)，會讓同步下來的目錄變成任何使用者都可寫入。
	// 實際結果會再被 umask 遮罩，所以在 umask 022 的環境下看起來沒差別，
	// 但服務以 umask 0 執行時 (部分 service manager、容器映像) 就會真的
	// 建出 world-writable 的目錄。與 log 資料夾採用同一組權限。
	syncDirPerm = 0755
)

// ctxWriter 讓傳輸中的寫入可被 context 中斷。
//
// sftp.File.WriteTo 會把 w.Write 的錯誤原樣往上回傳，因此在每個資料塊寫入前
// 檢查 context 即可讓下載中途停下來。中斷粒度是一個 SFTP 封包 (預設 32 KB)，
// 對大檔案而言已足夠即時。
//
// 包成 writer 而非改寫成分塊複製迴圈，是為了保留 WriteTo 內部的併發讀取。
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (cw *ctxWriter) Write(p []byte) (int, error) {
	if err := cw.ctx.Err(); err != nil {
		return 0, err
	}
	return cw.w.Write(p)
}

// ctxReader 是上傳方向的對應物，讓 sftp.File.ReadFrom 可被 context 中斷。
//
// 必須轉發 Stat()：ReadFrom 會以型別斷言取得來源大小來決定併發寫入的程度，
// 少了它會判定大小未知而退回單執行緒的循序路徑，白白損失吞吐量。
type ctxReader struct {
	ctx context.Context
	f   *os.File
}

func (cr *ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.f.Read(p)
}

func (cr *ctxReader) Stat() (os.FileInfo, error) { return cr.f.Stat() }

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

// globalTransferSem 是跨所有同步任務共用的傳輸名額。
//
// maxConcurrentTransfers 是「每個任務」各自的上限，而 cron 的每個 entry 都在
// 自己的 goroutine 執行 —— 二十幾台主機若排在同一個時間點，會一口氣開出
// 20 × 10 = 200 條併發傳輸，網路與磁碟被打滿的結果是每一台都變慢。
// 這道全域號誌把總量壓在 maxGlobalTransfers，讓任務之間互相排隊而非互相拖累。
//
// 之所以是套件層變數而非設定項：設定檔的格式是「任務陣列」，沒有放全域設定的
// 地方，加一層巢狀會破壞既有設定檔的相容性。
var globalTransferSem = make(chan struct{}, maxGlobalTransfers)

// transferLimiter 同時代表「單一任務」與「全域」兩道名額限制。
//
// 取號順序固定是先 task 再 global，所有呼叫端一致，因此不會有互等的死結。
type transferLimiter struct {
	task   chan struct{}
	global chan struct{}
}

func newTransferLimiter() *transferLimiter {
	return &transferLimiter{
		task:   make(chan struct{}, maxConcurrentTransfers),
		global: globalTransferSem,
	}
}

// acquire 依序取得兩道名額。取得 task 之後若因取消而拿不到 global，
// 必須把 task 的名額還回去，否則該次同步結束時會有名額永遠漏掉。
func (l *transferLimiter) acquire(ctx context.Context) error {
	if err := acquire(ctx, l.task); err != nil {
		return err
	}
	if err := acquire(ctx, l.global); err != nil {
		<-l.task
		return err
	}
	return nil
}

func (l *transferLimiter) release() {
	<-l.global
	<-l.task
}

// transferCounters 是一次同步作業共用的原子計數器
type transferCounters struct {
	succeeded uint64
	failed    uint64
	skipped   uint64 // 已是最新、無須傳輸
}

func (c *transferCounters) addSucceeded() { atomic.AddUint64(&c.succeeded, 1) }
func (c *transferCounters) addFailed()    { atomic.AddUint64(&c.failed, 1) }
func (c *transferCounters) addSkipped()   { atomic.AddUint64(&c.skipped, 1) }

// syncStats 是一次同步作業的結果摘要
type syncStats struct {
	Succeeded uint64
	Failed    uint64
	Skipped   uint64
	Duration  time.Duration
}

func (s syncStats) String() string {
	return fmt.Sprintf("成功 %d, 失敗 %d, 略過 %d, 耗時 %v",
		s.Succeeded, s.Failed, s.Skipped, s.Duration)
}

// syncData 執行一次同步並回傳結果摘要。
//
// 舊版只回傳「最上層目錄掃描」的錯誤，逐檔案的失敗全被吞進日誌，
// 呼叫端無從區分「完全成功」與「1000 個檔案失敗了 999 個」。
// 現在只要有任何檔案失敗就回傳錯誤，並一併給出統計數字。
//
// 因取消而中止時，回傳的錯誤會包住 ctx.Err()，
// 呼叫端可用 errors.Is(err, context.Canceled) 區分「被中斷」與「真的失敗」。
func syncData(ctx context.Context, client *sftp.Client, localDir, remoteDir, action string) (syncStats, error) {
	var wg sync.WaitGroup
	var counters transferCounters
	lim := newTransferLimiter()
	startTime := time.Now()

	log.Printf("[INFO] 開始同步作業 | 模式: %s | 來源/目的: %s <-> %s", action, localDir, remoteDir)

	var scanErr error
	switch action {
	case actionPull:
		scanErr = pullData(ctx, client, localDir, remoteDir, &wg, lim, &counters)
	case actionPush:
		scanErr = pushData(ctx, client, localDir, remoteDir, &wg, lim, &counters)
	default:
		return syncStats{}, fmt.Errorf("invalid action: %s", action)
	}

	wg.Wait() // 等待所有併發的檔案傳輸完成

	stats := syncStats{
		Succeeded: atomic.LoadUint64(&counters.succeeded),
		Failed:    atomic.LoadUint64(&counters.failed),
		Skipped:   atomic.LoadUint64(&counters.skipped),
		Duration:  time.Since(startTime),
	}
	log.Printf("[STAT] 同步作業結束 | 模式: %s | %s", action, stats)

	switch {
	case scanErr != nil:
		// 掃描階段就中止 (含 context 取消)。用 %w 包住，
		// 讓呼叫端能以 errors.Is 辨識取消。
		return stats, fmt.Errorf("同步中止 (%s): %w", stats, scanErr)
	case stats.Failed > 0:
		return stats, fmt.Errorf("有 %d 個檔案傳輸失敗 (%s)", stats.Failed, stats)
	default:
		return stats, nil
	}
}

// 單一檔案傳輸的最大嘗試次數 (含第一次)
const maxTransferAttempts = 3

// retryBaseDelay 是重試的基礎延遲，之後以 2 的次方遞增 (1s, 2s)。
// 宣告為 var 是為了讓測試能縮短等待時間。
var retryBaseDelay = time.Second

// isRetryable 判斷錯誤是否值得重試。
//
// 只重試可能是暫時性的問題 (網路抖動、連線被重置)。
// 來源檔不存在或權限不足屬於確定性錯誤，重試只是浪費時間與日誌。
func isRetryable(err error) bool {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
		return false
	default:
		return true
	}
}

// transferWithRetry 以指數退避重試單一檔案的傳輸。
//
// 舊版遇到暫時性的網路抖動就直接放棄該檔案，要等下一輪 cron 才會再試；
// 手動同步模式下則是完全不會再試。
//
// context 一旦取消就立即放棄 —— 服務正在關閉時繼續重試只會拖慢關閉，
// 而連線中斷時 KeepAlive 也會取消 context，因此不會對死連線空轉。
func transferWithRetry(ctx context.Context, desc string, transfer func() error) error {
	var err error

	for attempt := 1; attempt <= maxTransferAttempts; attempt++ {
		if err = transfer(); err == nil {
			return nil
		}
		if ctx.Err() != nil || !isRetryable(err) {
			return err
		}
		if attempt == maxTransferAttempts {
			break
		}

		delay := retryBaseDelay << (attempt - 1)
		log.Printf("[WARN] %s 第 %d/%d 次嘗試失敗，%v 後重試: %v",
			desc, attempt, maxTransferAttempts, delay, err)

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return err
		}
	}

	return fmt.Errorf("重試 %d 次後仍失敗: %w", maxTransferAttempts, err)
}

func pullData(ctx context.Context, client *sftp.Client, localDir, remoteDir string, wg *sync.WaitGroup, lim *transferLimiter, counters *transferCounters) error {
	remoteFiles, err := client.ReadDir(remoteDir)
	if err != nil {
		return err
	}

	// 目的地的最上層目錄也要自己建：舊版只在遞迴時替「掃到的子目錄」建目錄，
	// 最上層則假設已經存在 —— 日期資料夾模式下這代表每一天都得先手動建好本地
	// 資料夾，否則該日的每個檔案都會以 no such file or directory 失敗。
	//
	// 順序刻意放在 ReadDir 之後：先建目錄的話，遠端還沒產生的日期會在本地留下
	// 一堆空資料夾。
	if err := os.MkdirAll(localDir, syncDirPerm); err != nil {
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
			if err := os.MkdirAll(localFilePath, syncDirPerm); err != nil {
				log.Printf("[ERROR] 無法建立本地目錄 %s: %v", localFilePath, err)
				continue
			}
			// 遞迴時把計數器往下傳
			if err := pullData(ctx, client, localFilePath, remoteFilePath, wg, lim, counters); err != nil {
				log.Printf("[ERROR] 無法讀取遠端目錄 %s: %v", remoteFilePath, err)
				counters.addFailed()
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
				counters.addFailed()
				continue
			}
			if !transfer {
				counters.addSkipped()
				continue
			}

			// 先取號、再開 goroutine：號誌限制的必須是「實際存在的 goroutine 數」。
			// 舊版在 goroutine 內部才取號，大目錄會一次生出數十萬個阻塞中的
			// goroutine，記憶體直接爆掉。
			if err := lim.acquire(ctx); err != nil {
				log.Println("[INFO] 收到中斷訊號，停止派發後續檔案...")
				return err
			}

			wg.Add(1)
			go func(remote, local string, modTime time.Time) {
				defer wg.Done()
				defer lim.release()

				err := transferWithRetry(ctx, remote, func() error {
					return downloadFile(ctx, client, local, remote, modTime)
				})
				if err != nil {
					log.Printf("[FAIL] 檔案下載失敗 %s: %v", remote, err)
					counters.addFailed()
				} else {
					counters.addSucceeded()
				}
			}(remoteFilePath, localFilePath, file.ModTime())
		}
	}

	return nil
}

// 函數簽名加入 successCount 與 failedCount 的指標
func pushData(ctx context.Context, client *sftp.Client, localDir, remoteDir string, wg *sync.WaitGroup, lim *transferLimiter, counters *transferCounters) error {
	localFiles, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}

	// 與 pullData 對稱：最上層的遠端目錄同樣要自己建立
	if err := client.MkdirAll(remoteDir); err != nil {
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
				counters.addFailed()
				continue
			}
			// 遞迴時把計數器往下傳
			if err := pushData(ctx, client, localFilePath, remoteFilePath, wg, lim, counters); err != nil {
				log.Printf("[ERROR] 無法讀取本地目錄 %s: %v", localFilePath, err)
				counters.addFailed()
				continue
			}
		} else {
			localFileInfo, err := os.Stat(localFilePath)
			if err != nil {
				log.Printf("[ERROR] 無法取得本地檔案資訊 %s: %v", localFilePath, err)
				counters.addFailed()
				continue
			}

			remoteFileInfo, statErr := client.Stat(remoteFilePath)
			transfer, err := needsTransfer(localFileInfo, remoteFileInfo, statErr)
			if err != nil {
				// 連線中斷時 client.Stat 回的是網路錯誤而非 ENOENT，
				// 舊版會在此對 nil 解引用而讓整個服務 panic。
				log.Printf("[ERROR] 無法取得遠端檔案資訊 %s: %v", remoteFilePath, err)
				counters.addFailed()
				continue
			}
			if !transfer {
				counters.addSkipped()
				continue
			}

			// 先取號、再開 goroutine (原因同 pullData)
			if err := lim.acquire(ctx); err != nil {
				log.Println("[INFO] 收到中斷訊號，停止派發後續檔案...")
				return err
			}

			wg.Add(1)
			go func(local, remote string, modTime time.Time) {
				defer wg.Done()
				defer lim.release()

				err := transferWithRetry(ctx, local, func() error {
					return uploadFile(ctx, client, local, remote, modTime)
				})
				if err != nil {
					log.Printf("[FAIL] 檔案上傳失敗 %s: %v", local, err)
					counters.addFailed()
				} else {
					counters.addSucceeded()
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
func downloadFile(ctx context.Context, client *sftp.Client, localFilePath, remoteFilePath string, remoteModTime time.Time) error {
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

	// 透過 ctxWriter 寫入，讓傳輸中途可被取消 (服務停止 / Ctrl+C / 連線中斷)
	if _, err := remoteFile.WriteTo(&ctxWriter{ctx: ctx, w: localFile}); err != nil {
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
func uploadFile(ctx context.Context, client *sftp.Client, localFilePath, remoteFilePath string, localModTime time.Time) error {
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
	// 包一層 ctxReader 讓傳輸中途可被取消 (ctxReader 有轉發 Stat，併發度不受影響)。
	if _, err := remoteFile.ReadFrom(&ctxReader{ctx: ctx, f: localFile}); err != nil {
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

// syncDates 決定這次要同步哪些日期資料夾。
//
// 回傳 nil 代表不套用日期資料夾，直接同步 localDir ↔ remoteDir。
// 手動指定的日期優先於設定檔的 syncDays：前者是使用者當下的明確意圖。
func syncDates(config Config, startDate, endDate string, now time.Time) ([]string, error) {
	if startDate != "" && endDate != "" {
		return generateDateSlice(startDate, endDate)
	}
	return recentDateSlice(now, config.SyncDays), nil
}

func syncFolder(ctx context.Context, config Config, startDate, endDate string) {
	// 先算日期再連線：日期字串有問題時不必白白建立一次 SSH 連線
	dates, err := syncDates(config, startDate, endDate, time.Now())
	if err != nil {
		log.Println("Failed to generate date slice:", err)
		return
	}

	configSSH := createSSHConfig(config.User, config.Password)
	conn, err := connectToSSHServer(ctx, config.SSHHost, config.SSHPort, configSSH)
	if err != nil {
		log.Println(err)
		return
	}
	defer conn.Close()
	client, err := createNewClinet(conn.client)
	if err != nil {
		log.Println(err)
		return
	}
	defer client.Close()

	// 以下一律使用 conn.ctx 而非傳入的 ctx：除了原本的服務停止 / Ctrl+C，
	// 連線中斷時 KeepAlive 也會取消它，讓同步立刻收手。
	syncCtx := conn.ctx

	if len(dates) == 0 {
		if _, err := syncData(syncCtx, client, config.LocalDir, config.RemoteDir, config.Action); err != nil {
			log.Printf("[ERROR] 同步未完全成功 (%s): %v", config.RemoteDir, err)
		}
		return
	}

	for _, date := range dates {
		// 已取消就別再跑後續日期，否則會逐一失敗刷一堆日誌
		if err := syncCtx.Err(); err != nil {
			log.Println("[WARN] 同步已中斷，略過剩餘日期")
			return
		}

		remoteDir := path.Join(config.RemoteDir, date)
		localDir := filepath.Join(config.LocalDir, date)
		log.Println("Syncing Date:", date)
		_, err := syncData(syncCtx, client, localDir, remoteDir, config.Action)
		switch {
		case err == nil:
		case errors.Is(err, fs.ErrNotExist):
			// 來源的日期資料夾還不存在。syncDays 每小時跑一輪時，今天的資料夾
			// 在當天稍早本來就可能尚未產生 —— 這是正常狀態，記成 ERROR 會讓
			// 每台主機每小時都刷一行假錯誤。
			//
			// 只有「最上層那次 ReadDir」的錯誤會傳到這裡 (子目錄與個別檔案的
			// 失敗都併進統計)，因此不會誤把真正的傳輸失敗吞掉。
			log.Printf("[SKIP] 日期 %s 的來源資料夾不存在，略過", date)
		default:
			log.Printf("[ERROR] 日期 %s 同步未完全成功: %v", date, err)
		}
	}
}

func main() {
	svcConfig := &service.Config{
		Name:        "DataSyncService",
		DisplayName: "Data Sync Service",
		Description: "Synchronizes files between local and remote directories over SFTP. Each sync task runs on its own cron schedule defined in configs.json.",
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

		// 明確指定的日期會蓋掉設定檔的 syncDays。這是刻意的優先順序，
		// 但不能默默發生 —— 否則使用者會以為補的是 syncDays 那幾天。
		if n := countConfigsWithSyncDays(targets); n > 0 {
			log.Printf("[INFO] 有 %d 筆設定設有 syncDays，本次改以 -startDate/-endDate 指定的範圍為準", n)
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
