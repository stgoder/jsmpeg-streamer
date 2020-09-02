package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
)

type player struct {
	Key  string          `json:"key"`
	Time time.Time       `json:"time"`
	Conn *websocket.Conn `json:"-"`
}

type streamer struct {
	Key        string             `json:"key"`
	Source     string             `json:"source"`
	Resolution string             `json:"resolution"`
	Alive      bool               `json:"alive"`
	Cmd        *exec.Cmd          `json:"-"`
	PlayerMap  map[string]*player `json:"playerMap"`
}

func (s *streamer) tryStart() {
	if s.Alive {
		return
	}
	s.Alive = true

	params := []string{"ffmpeg"}
	isFile, _ := pathExists(s.Source)
	if !isFile {
		params = append(params, []string{"-rtsp_transport", "tcp"}...)
	}
	params = append(params, []string{"-re", "-i", s.Source,
		"-f", "mpegts", "-codec:v", "mpeg1video", "-nostats", "-r", "24", "-b:v", "700k"}...)
	if s.Resolution != "" {
		params = append(params, []string{"-s", s.Resolution}...)
	}
	params = append(params, []string{"-", "-loglevel", "error"}...)

	cmd := exec.Command(params[0], params[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Println(err)
		s.Alive = false
		return
	}
	cmd.Stderr = cmd.Stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err = cmd.Start(); err != nil {
		fmt.Println(err)
		s.Alive = false
		return
	}

	s.Cmd = cmd

	go func(s *streamer) {
		buf := make([]byte, 1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 && err == nil {
				for _, player := range s.PlayerMap {
					player.Conn.WriteMessage(websocket.BinaryMessage, buf[0:n])
				}
			}
			if err != nil {
				break
			}
		}
		s.Alive = false
		fmt.Println("cmd killed")
	}(s)
}

func (s *streamer) stop() {
	if s != nil {
		if s.Cmd != nil {
			err := s.Cmd.Process.Kill()
			if err != nil {
				fmt.Println("streamer cmd kill err: " + err.Error())
			}
		}
		s.Alive = false
	}
}

var streamerMap = make(map[string]*streamer)

var db *sql.DB

var ffmpegPath string

// go build -ldflags="-w -s"
// go build -ldflags="-w -s -H windowsgui"
func main() {
	root, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		fmt.Println("err root path: " + err.Error())
		return
	}

	var port int

	flag.IntVar(&port, "p", 10019, "端口, 默认10019")
	flag.StringVar(&ffmpegPath, "ff", "", "ffmpeg路径, 默认同级目录或环境变量")
	flag.Parse()

	fmt.Println("start at port " + strconv.Itoa(port))

	if ffmpegPath == "" {
		fmt.Println("未指定ffmepg路径, 查找同级目录")
		if runtime.GOOS == "windows" {
			ffmpegPath = root + string(os.PathSeparator) + "ffmpeg.exe"
		} else {
			ffmpegPath = root + string(os.PathSeparator) + "ffmpeg"
		}
		exists, _ := pathExists(ffmpegPath)
		if exists {
			fmt.Println("使用同级目录: " + ffmpegPath)
		} else {
			ffmpegPath = "ffmpeg"
			fmt.Println("同级目录未找到")
			fmt.Println("使用环境变量: " + ffmpegPath)
		}
	}

	// db sqlite3
	db, err = sql.Open("sqlite3", root+string(os.PathSeparator)+"data.db")
	if err != nil {
		fmt.Println("load data.db err: " + err.Error())
		return
	}

	sql1 := `create table if not exists "streamer" (
		"key" varchar(64) primary key not null,
		"source" varchar(256) not null,
		"resolution" varchar(16)
		);`
	_, err = db.Exec(sql1)
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	rows, err := db.Query("select key, source, resolution from streamer")
	if err != nil {
		fmt.Println("load streamers from db err: " + err.Error())
		return
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var source string
		var resolution string
		err = rows.Scan(&key, &source, &resolution)
		if err != nil {
			db.Close()
			fmt.Println(err.Error())
			return
		}
		streamerMap[key] = &streamer{
			Key:        key,
			Source:     source,
			Resolution: resolution,
			Alive:      false,
			Cmd:        nil,
			PlayerMap:  make(map[string]*player),
		}
	}
	err = rows.Err()
	if err != nil {
		db.Close()
		fmt.Println(err.Error())
		return
	}

	// web views
	fs := http.FileServer(http.Dir(root + string(os.PathSeparator) + "www"))
	http.Handle("/", http.StripPrefix("/", fs))

	// WebSocket realy
	var upgrader = websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		Subprotocols: []string{"null"},
	}
	http.HandleFunc("/relay", func(w http.ResponseWriter, r *http.Request) {
		uri := r.RequestURI
		fmt.Println(uri)
		key := r.URL.Query().Get("key")
		if key == "" {
			fmt.Println("err stream key")
			return
		}
		s := streamerMap[key]
		if s == nil {
			fmt.Println("err stream key")
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			fmt.Println(err)
			return
		}

		playerKey := conn.RemoteAddr().String()
		fmt.Println("player connected: " + playerKey)
		s.PlayerMap[playerKey] = &player{
			Key:  playerKey,
			Time: time.Now(),
			Conn: conn,
		}
		s.tryStart()

		go func(conn *websocket.Conn) {
			for {
				_, _, err := conn.ReadMessage()
				if err != nil {
					conn.Close()
					break
				}
			}
			fmt.Println("conn closed")
			playerKey := conn.RemoteAddr().String()
			fmt.Println(playerKey)
			for _, s := range streamerMap {
				if s != nil {
					delete(s.PlayerMap, playerKey)
					if len(s.PlayerMap) == 0 {
						s.stop()
						fmt.Println("streamer has no player, stop")
					}
				}
			}
		}(conn)
	})

	http.HandleFunc("/streamer/add", func(w http.ResponseWriter, r *http.Request) {
		var key string
		var source string
		var resolution string
		if r.Method == "GET" {
			q := r.URL.Query()
			key = q.Get("key")
			source = q.Get("source")
			resolution = q.Get("resolution")
		} else {
			key = r.FormValue("key")
			source = r.FormValue("source")
			resolution = r.FormValue("resolution")
		}
		key = strings.TrimSpace(key)
		source = strings.TrimSpace(source)
		resolution = strings.TrimSpace(resolution)
		if key == "" {
			w.Write([]byte("key required"))
			return
		}
		if source == "" {
			w.Write([]byte("source required"))
			return
		}
		s := streamerMap[key]
		if s != nil {
			w.Write([]byte("same key"))
			return
		}

		row := db.QueryRow("select count(*) from streamer where key = ?", key)
		var n int
		err = row.Scan(&n)
		if err != nil {
			w.Write([]byte(err.Error()))
			return
		}
		if n > 0 {
			w.Write([]byte("same key"))
			return
		}

		stmt, err := db.Prepare("insert into streamer(key, source, resolution) values(?,?,?)")
		if err != nil {
			w.Write([]byte(err.Error()))
			return
		}
		defer stmt.Close()
		_, err = stmt.Exec(key, source, resolution)
		if err != nil {
			w.Write([]byte(err.Error()))
			return
		}

		s = &streamer{
			Key:        key,
			Source:     source,
			Resolution: resolution,
			Alive:      false,
			Cmd:        nil,
			PlayerMap:  make(map[string]*player),
		}

		//streamer.Start()

		streamerMap[key] = s
		w.Write([]byte("ok"))
	})

	http.HandleFunc("/streamer/list", func(w http.ResponseWriter, r *http.Request) {
		list := make([]*streamerModel, 0)
		for _, s := range streamerMap {
			players := make([]*player, 0)
			for _, p := range s.PlayerMap {
				players = append(players, p)
			}
			sm := &streamerModel{
				Key:        s.Key,
				Source:     s.Source,
				Resolution: s.Resolution,
				Alive:      s.Alive,
				Players:    players,
			}
			list = append(list, sm)
		}
		b, err := json.Marshal(list)
		if err != nil {
			w.Write([]byte(err.Error()))
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		}
	})

	http.HandleFunc("/streamer/delete", func(w http.ResponseWriter, r *http.Request) {
		var key string
		if r.Method == "GET" {
			key = r.URL.Query().Get("key")
		} else {
			key = r.FormValue("key")
		}
		s := streamerMap[key]
		if s != nil {
			delete(streamerMap, key)
			s.stop()
		}

		stmt, err := db.Prepare("delete from streamer where key = ?")
		defer stmt.Close()
		_, err = stmt.Exec(key)
		if err != nil {
			w.Write([]byte(err.Error()))
			return
		}

		w.Write([]byte("ok"))
	})

	http.ListenAndServe("0.0.0.0:"+strconv.Itoa(port), nil)

}

type streamerModel struct {
	Key        string    `json:"key"`
	Source     string    `json:"source"`
	Resolution string    `json:"resolution"`
	Alive      bool      `json:"alive"`
	Players    []*player `json:"players"`
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
