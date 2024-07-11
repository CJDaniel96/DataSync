package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kardianos/service"
	"github.com/pkg/sftp"
	"github.com/robfig/cron/v3"
	"golang.org/x/crypto/ssh"
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

type program struct{}

var configs []Config

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *program) Stop(s service.Service) error {
	return nil
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
	log.Println("Configs read successfully")
	log.Println("Starting sync service")
	log.Println("Syncing every 30 minutes")

	c := cron.New()

	var wg sync.WaitGroup
	for _, config := range configs {
		wg.Add(1)
		go func(cfg Config) {
			defer wg.Done()
			c.AddFunc(cfg.Cron, func() {
				log.Println("Syncing folder: ", cfg.RemoteDir)
				syncFolder(cfg, "", "")
			})
		}(config)
	}
	c.Start()
	wg.Wait()
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

func connectToSFTPServer(host string, port int, config *ssh.ClientConfig) (*sftp.Client, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, err
	}
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

func syncData(client *sftp.Client, localDir, remoteDir, action string) error {
	if action == "pull" {
		return pullData(client, localDir, remoteDir)
	} else if action == "push" {
		return pushData(client, localDir, remoteDir)
	} else {
		return fmt.Errorf("invalid action: %s", action)
	}
}

func pullData(client *sftp.Client, localDir, remoteDir string) error {
	remoteFiles, err := client.ReadDir(remoteDir)
	if err != nil {
		return err
	}

	for _, file := range remoteFiles {
		remoteFilePath := filepath.Join(remoteDir, file.Name())
		localFilePath := filepath.Join(localDir, file.Name())

		if file.IsDir() {
			if err := os.MkdirAll(localFilePath, os.ModePerm); err != nil {
				log.Println("Failed to create local directory", localFilePath, ":", err)
				continue
			}
			if err := pullData(client, localFilePath, remoteFilePath); err != nil {
				log.Println("Failed to download directory", remoteFilePath, ":", err)
				continue
			}
		} else {
			remoteFileInfo, err := client.Stat(remoteFilePath)
			if err != nil {
				log.Println("Failed to stat remote file", remoteFilePath, ":", err)
				continue
			}

			localFileInfo, err := os.Stat(localFilePath)
			if os.IsNotExist(err) || remoteFileInfo.ModTime().After(localFileInfo.ModTime()) {
				if err := downloadFile(client, localFilePath, remoteFilePath); err != nil {
					log.Println("Failed to download file", remoteFilePath, ":", err)
				}
			}
		}
	}

	return nil
}

func pushData(client *sftp.Client, localDir, remoteDir string) error {
	localFiles, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}

	for _, file := range localFiles {
		localFilePath := filepath.Join(localDir, file.Name())
		remoteFilePath := filepath.Join(remoteDir, file.Name())

		if file.IsDir() {
			if err := client.MkdirAll(remoteFilePath); err != nil {
				log.Println("Failed to create remote directory", remoteFilePath, ":", err)
				continue
			}
			if err := pushData(client, localFilePath, remoteFilePath); err != nil {
				log.Println("Failed to upload directory", localFilePath, ":", err)
				continue
			}
		} else {
			localFileInfo, err := os.Stat(localFilePath)
			if err != nil {
				log.Println("Failed to stat local file", localFilePath, ":", err)
				continue
			}

			remoteFileInfo, err := client.Stat(remoteFilePath)
			if os.IsNotExist(err) || localFileInfo.ModTime().After(remoteFileInfo.ModTime()) {
				if err := uploadFile(client, localFilePath, remoteFilePath); err != nil {
					log.Println("Failed to upload file", localFilePath, ":", err)
				}
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

func syncFolder(config Config, startDate, endDate string) {
	configSSH := createSSHConfig(config.User, config.Password)
	client, err := connectToSFTPServer(config.SSHHost, config.SSHPort, configSSH)
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
			remoteDir := filepath.Join(config.RemoteDir, date)
			localDir := filepath.Join(config.LocalDir, date)
			action := config.Action
			log.Println("Syncing Date:", date)
			if err := syncData(client, localDir, remoteDir, action); err != nil {
				log.Println("Failed to sync folder:", err)
			}
		}
	} else {
		remoteDir := config.RemoteDir
		localDir := config.LocalDir
		action := config.Action
		if err := syncData(client, localDir, remoteDir, action); err != nil {
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
	configPath := filepath.Join(exeDir, "data_sync_configs.json")
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
		log.Println("Syncing folders with date range")
		for _, config := range configs {
			log.Println("Syncing folder: ", config.RemoteDir)
			syncFolder(config, *startDate, *endDate)
		}
		log.Println("Syncing completed")
		return
	}

	if err := s.Run(); err != nil {
		log.Fatal(err)
	}
}
