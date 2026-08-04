# Data Sync Service

透過 SFTP 在本機目錄與遠端目錄之間同步檔案的服務。可以安裝成系統服務依 cron
排程自動執行，也可以從命令列針對指定日期範圍手動補資料。

## 運作模式

| 模式 | 觸發方式 | 同步範圍 |
| --- | --- | --- |
| 排程服務 | 安裝成系統服務後常駐 | `localDir` ↔ `remoteDir` |
| 手動同步 | 命令列帶 `-startDate` / `-endDate` | `localDir/<日期>` ↔ `remoteDir/<日期>` |

兩者共用同一份設定檔。手動模式會在 `localDir` 與 `remoteDir` 後各自接上日期
子資料夾（`2026-08-01` 這種格式），適合處理「每天一個資料夾」的目錄結構。

## 建置

需要 Go 1.24 以上。

```bash
go build -o data_sync
```

Windows：

```bash
go build -o data_sync.exe
```

## 設定檔

設定檔必須命名為 `configs.json`，並放在**執行檔所在的目錄**（不是當前工作目錄）。
程式沒有指定設定檔路徑的參數。

```json
[
    {
        "sshHost": "example.com",
        "sshPort": 22,
        "user": "username",
        "password": "password",
        "localDir": "C:\\data\\local",
        "remoteDir": "/data/remote",
        "cron": "0 * * * *",
        "action": "pull"
    }
]
```

檔案內容是一個陣列，每個元素代表一項獨立的同步任務。同一台主機可以有多筆設定，
對應不同的路徑。

### 欄位說明

| 欄位 | 型別 | 說明 |
| --- | --- | --- |
| `sshHost` | string | SSH 伺服器主機名稱或 IP，不可為空 |
| `sshPort` | int | SSH 連接埠，須介於 1–65535 |
| `user` | string | SSH 帳號，不可為空 |
| `password` | string | SSH 密碼 |
| `localDir` | string | 本機目錄，不可為空 |
| `remoteDir` | string | 遠端目錄，不可為空 |
| `cron` | string | 排程表達式，見下方說明 |
| `action` | string | 只接受 `pull` 或 `push`（大小寫需相符） |

- `pull`：遠端 → 本機
- `push`：本機 → 遠端

### cron 表達式

採用標準 5 欄位格式（分 時 日 月 週），也支援描述子：

```
0 * * * *        每小時整點
*/15 * * * *     每 15 分鐘
30 2 * * *       每天 02:30
@every 30m       每 30 分鐘
@daily           每天午夜
```

### 設定驗證

程式在啟動時就會驗證整份設定，任何一筆不合法都會中止並列出**所有**問題，不會
帶著錯誤的設定啟動：

```
無法載入設定檔 /opt/datasync/configs.json: 設定內容不合法:
  - 第 2 筆 (remoteDir="/r2"): sshHost 不可為空; sshPort 99999 超出有效範圍 (1-65535); action "sync" 無效，必須是 "pull" 或 "push"
```

空陣列 `[]` 也視為錯誤——否則服務會啟動後永遠閒置，看起來卻一切正常。

## 使用方式

### 服務管理

```bash
./data_sync install      # 安裝為系統服務
./data_sync start        # 啟動服務
./data_sync stop         # 停止服務
./data_sync uninstall    # 移除服務
```

服務名稱為 `DataSyncService`，顯示名稱為 `Data Sync Service`。安裝與啟動通常
需要系統管理員權限（Windows 需以系統管理員身分執行，Linux 需 `sudo`）。

> **注意**：包含 `install` 在內的所有子命令都會先載入並驗證 `configs.json`。
> 請先把設定檔準備好再執行 `install`，否則會直接中止。

不帶任何參數直接執行時，程式會以服務模式在前景運行，方便除錯。

### 手動同步指定日期範圍

`-startDate` 與 `-endDate` 兩者都要給，格式為 `YYYY-MM-DD`，區間包含頭尾兩天。

```bash
./data_sync -startDate=2026-08-01 -endDate=2026-08-03
```

日期資料夾**由新到舊**依序處理。作業若中途以 Ctrl+C 中斷，已完成的會是最近期
的資料。

### 只同步特定主機

設定檔有多台主機時，`-host` 可以只跑其中一台底下的所有設定：

```bash
./data_sync -startDate=2026-08-01 -endDate=2026-08-03 -host=example.com
```

- 主機名稱不分大小寫，前後空白會自動忽略
- 只做完整比對，不會用 `example` 誤中 `example.com`
- 找不到相符主機時會中止並列出設定檔中可用的主機清單
- 此旗標只作用於手動同步模式，未搭配日期範圍時會直接報錯，不會被靜默忽略

查看所有可用參數：

```bash
./data_sync -h
```

## 同步行為

### 判斷是否需要傳輸

符合以下任一條件就傳輸，否則跳過：

- 目的地不存在
- 檔案大小不同
- 來源的修改時間比目的地新

傳輸完成後會把目的地的修改時間對齊來源，因此同一份資料重複執行不會重傳。
比較大小而非只看時間，也能自動修復先前傳輸中斷所留下的不完整檔案。

### 傳輸過程

檔案會先寫入 `.part` 暫存檔，完整寫入後才更名為正式檔名。傳輸失敗或中斷時
暫存檔會被清除，不會在目的地留下內容不完整的檔案。掃描時會自動略過 `.part`
檔案。

### 其他行為

- 遞迴同步子目錄，缺少的目錄會自動建立
- 符號連結一律略過（兩個方向皆是）
- 每個同步任務最多同時傳輸 10 個檔案（此上限是各任務獨立計算，非全域）
- 前一輪同步尚未結束時，下一個排程時間點會直接跳過，不會併發執行同一組目錄
- SSH 連線逾時 30 秒，連線建立後每 30 秒送一次 keepalive
- 每輪結束會輸出成功／失敗檔案數與總耗時統計

## 日誌

日誌寫入執行檔所在目錄下的 `log/` 資料夾，檔名為 `sync_service_<日期>.log`。

- 跨日自動切換到新檔案
- 單檔超過 10 MB 會輪替，同一天最多保留 7 個輪替檔
- 舊日誌會壓縮成 `.gz`
- 超過 30 天的日誌會自動刪除

啟動階段的錯誤（設定檔讀不到、格式錯誤、log 資料夾建立失敗）除了寫入日誌檔
外，也會同時輸出到 console 與系統日誌（Windows 事件檢視器 / Linux syslog），
避免服務起不來時完全查不到原因。

## 安全性注意事項

以下是目前實作的已知限制，部署前請評估：

- **不驗證伺服器主機金鑰。** 目前使用 `ssh.InsecureIgnoreHostKey()`，不會檢查
  遠端主機的身分，在不可信的網路環境中存在中間人攻擊風險。
- **密碼以明文儲存在 `configs.json`。** 程式不會檢查該檔案的權限，請自行限制
  存取（例如 `chmod 600`，或在 Windows 上調整 ACL）。

`.gitignore` 已排除 `*.json`，設定檔不會被意外提交。

## 開發

### 專案結構

```
main.go        程式全部的實作
main_test.go   單元測試
configs.json   設定檔（不納入版控，需自行建立）
log/           執行時自動產生
```

### 測試

```bash
go test -race ./...
```

## License

MIT，詳見 [LICENSE](LICENSE)。
