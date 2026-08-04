//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// 同步建立的本地目錄不得是 world-writable。
//
// 這個測試必須先把 umask 設為 0 才有意義: 一般環境的 umask (022) 會把 0777
// 遮罩成 0755，使 os.ModePerm 與 0755 的差別完全看不出來，測試會假通過。
// 真正會建出 world-writable 目錄的情境，正是服務以 umask 0 執行的時候。
func TestPullCreatesDirsWithoutWorldWrite(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	srv := newTestSFTPServer(t)
	client, conn := srv.dial(t)

	remote := t.TempDir()
	local := t.TempDir()
	writeFile(t, filepath.Join(remote, "sub", "nested", "a.txt"), []byte("x"))

	if _, err := syncData(conn.ctx, client, local, remote, actionPull); err != nil {
		t.Fatalf("syncData 失敗: %v", err)
	}

	for _, rel := range []string{"sub", filepath.Join("sub", "nested")} {
		info, err := os.Stat(filepath.Join(local, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		mode := info.Mode().Perm()
		if mode&0o002 != 0 {
			t.Errorf("%s 為 world-writable (%04o)", rel, mode)
		}
		if mode != syncDirPerm {
			t.Errorf("%s 權限 = %04o, 預期 %04o", rel, mode, syncDirPerm)
		}
	}
}
