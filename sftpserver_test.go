package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	testUser     = "testuser"
	testPassword = "testpass"
)

// testSFTPServer 是跑在同一個行程內的 SSH + SFTP 伺服器。
//
// 傳輸相關的行為 —— 取消、比對規則、.part 清理、連線中斷 —— 只靠單元測試
// 無法涵蓋，而依賴外部 SFTP server 又讓測試不可攜。這個伺服器讓這些路徑
// 能被自動化驗證。
//
// 它以真實檔案系統為後端 (sftp.NewServer 的預設行為)，因此 remoteDir 直接
// 用 t.TempDir() 的絕對路徑即可，斷言也就是一般的檔案檢查。
type testSFTPServer struct {
	addr     string
	listener net.Listener
	wg       sync.WaitGroup

	mu    sync.Mutex
	conns []net.Conn // 保留底層連線，供測試模擬網路中斷
}

func newTestSFTPServer(t *testing.T) *testSFTPServer {
	t.Helper()

	// ed25519 產生金鑰極快，避免每個測試都花時間在 RSA 上
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("產生測試主機金鑰失敗: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("建立 signer 失敗: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == testUser && string(pass) == testPassword {
				return nil, nil
			}
			return nil, errors.New("authentication failed")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("監聽失敗: %v", err)
	}

	s := &testSFTPServer{addr: ln.Addr().String(), listener: ln}
	s.wg.Add(1)
	go s.acceptLoop(cfg)

	t.Cleanup(s.Close)
	return s
}

func (s *testSFTPServer) acceptLoop(cfg *ssh.ServerConfig) {
	defer s.wg.Done()

	for {
		nConn, err := s.listener.Accept()
		if err != nil {
			return // listener 已關閉
		}

		s.mu.Lock()
		s.conns = append(s.conns, nConn)
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(nConn, cfg)
		}()
	}
}

func (s *testSFTPServer) serveConn(nConn net.Conn, cfg *ssh.ServerConfig) {
	defer nConn.Close()

	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return // 交握失敗或連線已被中斷
	}
	defer sshConn.Close()

	// keepalive@golang.org 這類全域請求會走到這裡。DiscardRequests 會回覆
	// false，而 client 端的 SendRequest 只有在連線壞掉時才回傳 error，
	// 因此這與真實伺服器的行為一致。
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session is supported")
			continue
		}

		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}

		go s.serveSession(ch, chReqs)
	}
}

func (s *testSFTPServer) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	for req := range reqs {
		// payload 是長度前綴字串，前 4 個位元組是長度
		isSFTP := req.Type == "subsystem" &&
			len(req.Payload) >= 4 && string(req.Payload[4:]) == "sftp"

		if req.WantReply {
			_ = req.Reply(isSFTP, nil)
		}
		if !isSFTP {
			continue
		}

		srv, err := sftp.NewServer(ch)
		if err != nil {
			return
		}
		// Serve 在 client 關閉或連線中斷時回傳，兩者在測試中都屬正常
		_ = srv.Serve()
		_ = srv.Close()
		return
	}
}

// breakConnections 直接關閉底層 TCP 連線，模擬網路中斷。
// 刻意不走 SSH 的正常關閉交握，讓 client 端的下一次 KeepAlive 失敗。
func (s *testSFTPServer) breakConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, c := range s.conns {
		_ = c.Close()
	}
}

func (s *testSFTPServer) Close() {
	_ = s.listener.Close()
	s.breakConnections()
	s.wg.Wait()
}

// config 產生一份指向本測試伺服器的 Config
func (s *testSFTPServer) config(t *testing.T, localDir, remoteDir, action string) Config {
	t.Helper()

	host, portStr, err := net.SplitHostPort(s.addr)
	if err != nil {
		t.Fatalf("解析測試伺服器位址失敗: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("解析測試伺服器連接埠失敗: %v", err)
	}

	return Config{
		SSHHost:   host,
		SSHPort:   port,
		User:      testUser,
		Password:  testPassword,
		LocalDir:  localDir,
		RemoteDir: remoteDir,
		Cron:      "@every 1h",
		Action:    action,
	}
}

// dial 以受測程式碼自己的連線路徑連上測試伺服器，
// 回傳 sftp client 與可觀察連線狀態的 sshConn。
func (s *testSFTPServer) dial(t *testing.T) (*sftp.Client, *sshConn) {
	t.Helper()

	cfg := s.config(t, "", "", actionPull)
	conn, err := connectToSSHServer(t.Context(), cfg.SSHHost, cfg.SSHPort,
		createSSHConfig(cfg.User, cfg.Password))
	if err != nil {
		t.Fatalf("連線至測試伺服器失敗: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client, err := sftp.NewClient(conn.client)
	if err != nil {
		t.Fatalf("建立 SFTP client 失敗: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	return client, conn
}
