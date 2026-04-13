package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
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

var configs []Config

type program struct {
	cron   *cron.Cron         // 將 cron 升級為結構屬性，方便在 Stop 中存取
	ctx    context.Context    // 用來向下傳遞取消訊號
	cancel context.CancelFunc // 用來觸發取消
	done   chan struct{}      // 用來等待主迴圈完全結束
}
 
func (p *program) Start(s service.Service) error {
	// 初始化 Context 與排程器
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.cron = cron.New()
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

func setupLogger(exeDir string) {
	// 1. 定義 log 資料夾的路徑 (在執行檔所在目錄下的 "log" 資料夾)
	logDir := filepath.Join(exeDir, "log")
 
	// 2. 檢查並自動建立 log 資料夾
	// os.MkdirAll 會在資料夾不存在時自動建立，如果已經存在則什麼都不做 (類似 Linux 的 mkdir -p)
	// 0755 是跨平台通用的資料夾權限設定
	if err := os.MkdirAll(logDir, 0755); err != nil {
		// 如果因為權限等問題無法建立資料夾，就在終端機印出錯誤並終止程式
		log.Fatalf("無法建立 log 資料夾: %v", err)
	}
 
	// 3. 定義日誌檔案的完整路徑 (現在它位於 log 資料夾內)
	logPath := filepath.Join(logDir, "sync_service.log")
 
	// 4. 讓 lumberjack 接管 log 的輸出
	log.SetOutput(&lumberjack.Logger{
		Filename:   logPath, // 指向 log/sync_service.log
		MaxSize:    10,      // 每個日誌檔最大容量 (單位：MB)
		MaxBackups: 7,       // 最多保留幾個舊的日誌檔
		MaxAge:     30,      // 舊日誌最多保留幾天
		Compress:   true,    // 是否壓縮舊日誌 (.gz)
	})
 
	// 設定日誌格式：加入日期與時間 (精確到微秒)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

func loadConfig(configPath string) error {
	file, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("unable to read config file: %w", err)
	}

	err = json.Unmarshal(file, &configs)
	if err != nil {
		return fmt.Errorf("unable to parse config JSON: %w", err)
	}

	return nil
}

func (p *program) run() {
	defer close(p.done) // 當 run 結束時，通知 Stop 可以放行了
	log.Println("[INFO] Configs read successfully")
	log.Println("[INFO] Starting sync service")
 
	for _, config := range configs {
		cfg := config // 重要：避免 Goroutine 閉包捕獲到最後一個變數
		p.cron.AddFunc(cfg.Cron, func() {
			log.Println("[INFO] 排程啟動，準備同步資料夾: ", cfg.RemoteDir)
			// 將 p.ctx 往下傳遞
			syncFolder(p.ctx, cfg, "", "") 
		})
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

func generateDateSlice(startDate, endDate string) ([]string, error) {
	var dateSlice []string
	start, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		return nil, err
	}
	end, err := time.Parse("2006-01-02", endDate)
	if err != nil {
		return nil, err
	}
	for !start.After(end) {
		dateSlice = append(dateSlice, start.Format("2006-01-02"))
		start = start.AddDate(0, 0, 1)
	}
	return dateSlice, nil
}

func syncData(ctx context.Context, client *sftp.Client, localDir, remoteDir, action string) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	// === 新增：原子計數器與時間紀錄 ===
	var successCount, failedCount uint64
	startTime := time.Now()
	log.Printf("[INFO] 開始同步作業 | 模式: %s | 來源/目的: %s <-> %s", action, localDir, remoteDir)
	// ===================================
 
	var err error
	if action == "pull" {
		err = pullData(ctx, client, localDir, remoteDir, &wg, sem, &successCount, &failedCount)
	} else if action == "push" {
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

		remoteFilePath := filepath.Join(remoteDir, file.Name())
		localFilePath := filepath.Join(localDir, file.Name())
 
		if file.Mode()&os.ModeSymlink != 0 {
			log.Printf("[SKIP] 略過符號連結 (pull): %s", remoteFilePath)
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
			remoteFileInfo, err := client.Stat(remoteFilePath)
			if err != nil {
				log.Printf("[ERROR] 無法取得遠端檔案資訊 %s: %v", remoteFilePath, err)
				continue
			}
 
			localFileInfo, err := os.Stat(localFilePath)
			if os.IsNotExist(err) || remoteFileInfo.ModTime().After(localFileInfo.ModTime()) {
				wg.Add(1)
				go func(remote, local string) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
 
					// 依據傳輸結果更新計數器
					if err := downloadFile(client, local, remote); err != nil {
						log.Printf("[FAIL] 檔案下載失敗 %s: %v", remote, err)
						atomic.AddUint64(failedCount, 1) // 失敗計數 +1
					} else {
						atomic.AddUint64(successCount, 1) // 成功計數 +1
					}
				}(remoteFilePath, localFilePath)
			}
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
		remoteFilePath := filepath.Join(remoteDir, file.Name())
 
		if file.Type()&os.ModeSymlink != 0 {
			log.Printf("[SKIP] 略過符號連結 (push): %s", localFilePath)
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
 
			remoteFileInfo, err := client.Stat(remoteFilePath)
			if os.IsNotExist(err) || localFileInfo.ModTime().After(remoteFileInfo.ModTime()) {
				wg.Add(1)
				go func(local, remote string) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
 
					// 依據傳輸結果更新計數器
					if err := uploadFile(client, local, remote); err != nil {
						log.Printf("[FAIL] 檔案上傳失敗 %s: %v", local, err)
						atomic.AddUint64(failedCount, 1) // 失敗計數 +1
					} else {
						atomic.AddUint64(successCount, 1) // 成功計數 +1
					}
				}(localFilePath, remoteFilePath)
			}
		}
	}
 
	return nil
}

func downloadFile(client *sftp.Client, localFilePath, remoteFilePath string) error {
	remoteFile, err := client.Open(remoteFilePath)
	if err != nil {
		return err
	}
	defer remoteFile.Close()

	localFile, err := os.Create(localFilePath)
	if err != nil {
		return err
	}
	defer localFile.Close()

	if _, err := remoteFile.WriteTo(localFile); err != nil {
		return err
	}

	log.Println("Downloaded", remoteFilePath, "to", localFilePath)
	return nil
}

func uploadFile(client *sftp.Client, localFilePath, remoteFilePath string) error {
	localFile, err := os.Open(localFilePath)
	if err != nil {
		return err
	}
	defer localFile.Close()

	remoteFile, err := client.Create(remoteFilePath)
	if err != nil {
		return err
	}
	defer remoteFile.Close()

	if _, err := localFile.WriteTo(remoteFile); err != nil {
		return err
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
		log.Fatal(err)
	}

	startDate := flag.String("startDate", "", "Start date for data sync")
	endDate := flag.String("endDate", "", "End date for data sync")
	flag.Parse()

	// Load configuration at service start
	exePath, err := os.Executable()
	if err != nil {
		log.Fatal("Failed to get executable path: ", err)
	}
	exeDir := filepath.Dir(exePath)

	setupLogger(exeDir)

	configPath := filepath.Join(exeDir, "configs.json")
	if err := loadConfig(configPath); err != nil {
		log.Fatal("Failed to load configuration: ", err)
	}

	if len(os.Args) > 1 {
		serviceAction := os.Args[1]
		switch serviceAction {
		case "install":
			if err := s.Install(); err != nil {
				log.Fatal(err)
			}
			log.Println("Service installed successfully")
			return
		case "uninstall":
			if err := s.Uninstall(); err != nil {
				log.Fatal(err)
			}
			log.Println("Service uninstalled successfully")
			return
		case "start":
			if err := s.Start(); err != nil {
				log.Fatal(err)
			}
			log.Println("Service started successfully")
			return
		case "stop":
			if err := s.Stop(); err != nil {
				log.Fatal(err)
			}
			log.Println("Service stopped successfully")
			return
		}
	}

	if *startDate != "" && *endDate != "" {
		log.Println("[INFO] 進入手動同步模式，根據指定日期範圍進行同步")
		// === 建立攔截 Ctrl+C 與系統終止訊號的 Context ===
		// NotifyContext 會在收到 os.Interrupt (Ctrl+C) 或 SIGTERM 時，自動觸發 ctx 的 Cancel
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop() // 確保程式結束時釋放監聽資源
		// ==============================================
 
		for _, config := range configs {
			log.Println("[INFO] 開始手動同步資料夾: ", config.RemoteDir)
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
		log.Fatal(err)
	}
}
