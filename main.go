package main

import (
	"bufio"
	"database/sql"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3" // SQLite 驱动
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var db *sql.DB
var outputDir string
var watchDir string
var defaultTracker string
var semaphore chan struct{}
var wg sync.WaitGroup
var dbMutex sync.Mutex

func init() {
	readConfig()
}

func readConfig() {
	file, err := os.Open("make-torrent.config")
	if err != nil {
		log.Fatal("Error opening config file:", err)
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			log.Fatal("Error closing config file:", err)
		}
	}(file)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			switch key {
			case "outputDir":
				outputDir = value
			case "watchDir":
				watchDir = value
			case "defaultTracker":
				defaultTracker = value
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Fatal("Error reading config file:", err)
	}

	if outputDir == "" || watchDir == "" {
		log.Fatal("outputDir and watchDir must be set in the config file")
	}
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "./processed_files.db")
	if err != nil {
		log.Fatal(err)
	}

	// 创建表（如果不存在）
	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS processed_files (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            path TEXT NOT NULL UNIQUE,
            processed_time DATETIME
        )
    `)
	if err != nil {
		log.Fatal(err)
	}
}

func updateDatabase(filePath string) {
	dbMutex.Lock()         // 获取互斥锁
	defer dbMutex.Unlock() // 函数返回时释放互斥锁

	// 更新数据库，记录已处理的文件
	_, err := db.Exec("INSERT INTO processed_files (path, processed_time) VALUES (?, ?)", filePath, time.Now())
	if err != nil {
		log.Println("Error updating database:", err)
	}
}

func main() {
	initDB()
	defer func(db *sql.DB) {
		err := db.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(db)

	maxThreads := runtime.NumCPU() / 2
	if maxThreads < 1 {
		maxThreads = 1
	}
	semaphore = make(chan struct{}, maxThreads)

	// 初始扫描监视文件夹中的所有子文件夹
	err := filepath.Walk(watchDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && path != watchDir {
			processNewFolder(path)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		log.Fatal("Error scanning watch directory:", err)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer func(watcher *fsnotify.Watcher) {
		err := watcher.Close()
		if err != nil {
			log.Fatal(err)
		}
	}(watcher)

	go watchFolders(watcher)

	err = watcher.Add(watchDir)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Watching directory:", watchDir)
	log.Println("Outputting torrents to:", outputDir)

	go func() {
		wg.Wait()
		log.Println("All torrent creation tasks completed.")
	}()

	select {}
}

func watchFolders(watcher *fsnotify.Watcher) {
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				if strings.HasSuffix(event.Name, ".torrent") {
					// 对于 .torrent 文件，只记录日志
					log.Println("Torrent file detected:", event.Name)
				} else {
					fi, err := os.Stat(event.Name)
					if err != nil {
						log.Println("Error getting file info:", err)
						continue
					}
					if fi.IsDir() {
						processNewFolder(event.Name)
					}
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("error:", err)
		}
	}
}

func processNewFolder(folderPath string) {
	if isNewFolder(folderPath) {
		wg.Add(1)
		semaphore <- struct{}{}
		go func() {
			defer func() {
				<-semaphore
				wg.Done()
			}()
			createTorrent(folderPath)
			updateDatabase(folderPath)
		}()
	}
}

func isNewFolder(folderPath string) bool {
	dbMutex.Lock()         // 获取互斥锁
	defer dbMutex.Unlock() // 函数返回时释放互斥锁

	// 检查数据库，判断是否为新文件夹
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM processed_files WHERE path = ?", folderPath).Scan(&count)
	if err != nil {
		log.Println("Database query error:", err)
		return false
	}
	return count == 0
}

func createTorrent(filePath string) {
	// 创建 MetaInfo 结构
	mi := &metainfo.MetaInfo{
		CreatedBy:    "xchgeaxeax-auto-torrent",
		CreationDate: time.Now().Unix(),
		Announce:     defaultTracker, // 设置默认的 tracker
	}

	// 创建 Info 结构
	info := metainfo.Info{
		PieceLength: 1024 * 1024, // 1 MB per piece
	}

	// 获取文件信息
	file, err := os.Open(filePath)
	if err != nil {
		log.Println("Error opening file:", err)
		return
	}
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			log.Println("Error closing file:", err)
		}
	}(file)

	fileInfo, err := file.Stat()
	if err != nil {
		log.Println("Error getting file info:", err)
		return
	}

	// 如果是单个文件
	if !fileInfo.IsDir() {
		info.Name = filepath.Base(filePath)
		info.Length = fileInfo.Size()
	} else {
		// 如果是目录，需要遍历所有文件
		info.Name = filepath.Base(filePath)
		err = filepath.Walk(filePath, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !fi.IsDir() {
				relPath, err := filepath.Rel(filePath, path)
				if err != nil {
					return err
				}
				info.Files = append(info.Files, metainfo.FileInfo{
					Path:   filepath.SplitList(relPath),
					Length: fi.Size(),
				})
			}
			return nil
		})
		if err != nil {
			log.Println("Error walking directory:", err)
			return
		}
	}

	// 计算 pieces
	err = info.GeneratePieces(func(fi metainfo.FileInfo) (io.ReadCloser, error) {
		return os.Open(filepath.Join(filePath, filepath.Join(fi.Path...)))
	})
	if err != nil {
		log.Println("Error generating pieces:", err)
		return
	}

	// 设置 info 字典
	mi.InfoBytes, err = bencode.Marshal(info)
	if err != nil {
		log.Println("Error marshalling info:", err)
		return
	}

	// 修改保存种子文件的路径
	torrentFileName := filepath.Base(filePath) + ".torrent"
	torrentPath := filepath.Join(outputDir, torrentFileName)
	torrentFile, err := os.Create(torrentPath)
	if err != nil {
		log.Println("Error creating torrent file:", err)
		return
	}
	defer func(torrentFile *os.File) {
		err := torrentFile.Close()
		if err != nil {
			log.Println("Error closing torrent file:", err)
		}
	}(torrentFile)

	err = mi.Write(torrentFile)
	if err != nil {
		log.Println("Error writing torrent file:", err)
		return
	}

	log.Println("Created torrent for:", filePath)
	log.Println("Torrent saved at:", torrentPath)
}
