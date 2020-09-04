package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"io"
	"log"
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
	"github.com/leaanthony/mewn"
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
	Lazy       bool               `json:"lazy"`
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
		log.Println(err)
		s.Alive = false
		return
	}
	cmd.Stderr = cmd.Stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err = cmd.Start(); err != nil {
		log.Println(err)
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
		log.Println("streamer " + s.Key + " cmd killed")
	}(s)
}

func (s *streamer) stop() {
	if s != nil {
		// if s.Cmd != nil && s.Alive {
		if s.Cmd != nil {
			err := s.Cmd.Process.Kill()
			if err != nil {
				log.Println("streamer cmd kill err: " + err.Error())
			}
		}
		s.Alive = false
	}
}

var streamerMap = make(map[string]*streamer)

var db *sql.DB

var port int
var ffmpegPath string

// go build -ldflags="-w -s"
// go build -ldflags="-w -s -H windowsgui"
// mewn build -ldflags="-w -s"
// mewn build -ldflags="-w -s -H windowsgui"
func main() {
	root, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Fatal(err)
	}

	logFile, err := os.OpenFile(root+string(os.PathSeparator)+"jsmpeg-streamer.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0766)
	if err != nil {
		log.Fatal(err)
	}
	defer logFile.Close()
	w := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	flag.IntVar(&port, "p", 10019, "端口, 默认10019")
	flag.StringVar(&ffmpegPath, "ff", "", "ffmpeg路径, 默认同级目录或环境变量")
	flag.Parse()

	log.Println("using port " + strconv.Itoa(port))

	if ffmpegPath == "" {
		log.Println("未指定ffmepg路径, 查找同级目录")
		if runtime.GOOS == "windows" {
			ffmpegPath = root + string(os.PathSeparator) + "ffmpeg.exe"
		} else {
			ffmpegPath = root + string(os.PathSeparator) + "ffmpeg"
		}
		exists, _ := pathExists(ffmpegPath)
		if exists {
			log.Println("使用同级目录: " + ffmpegPath)
		} else {
			ffmpegPath = "ffmpeg"
			log.Println("同级目录未找到")
			log.Println("使用环境变量: " + ffmpegPath)
		}
	}

	// try exec ffmpeg
	log.Println("测试执行 ffmpeg -version")
	cmd := exec.Command(ffmpegPath, "-version")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err = cmd.Start(); err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 2048)
	for {
		n, err := stdout.Read(buf)
		if n > 0 && err == nil {
			log.Print(string(buf[0:n]))
		}
		if err != nil {
			break
		}
	}
	log.Println("exec " + ffmpegPath + " ok")

	// db sqlite3
	db, err = sql.Open("sqlite3", root+string(os.PathSeparator)+"data.db")
	if err != nil {
		log.Fatalln("load data.db err: " + err.Error())
	}

	sql1 := `create table if not exists "streamer" (
		"key" varchar(64) primary key not null,
		"source" varchar(256) not null,
		"resolution" varchar(16),
		"lazy" tinyint(1) not null
		);`
	_, err = db.Exec(sql1)
	if err != nil {
		log.Fatalln(err.Error())
	}

	rows, err := db.Query("select key, source, resolution, lazy from streamer")
	if err != nil {
		log.Fatalln("load streamers from db err: " + err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var source string
		var resolution string
		var lazy bool
		err = rows.Scan(&key, &source, &resolution, &lazy)
		if err != nil {
			db.Close()
			log.Fatalln(err.Error())
		}
		s := &streamer{
			Key:        key,
			Source:     source,
			Resolution: resolution,
			Lazy:       lazy,
			Alive:      false,
			Cmd:        nil,
			PlayerMap:  make(map[string]*player),
		}
		streamerMap[key] = s
		if !s.Lazy {
			s.tryStart()
		}
	}
	err = rows.Err()
	if err != nil {
		db.Close()
		log.Fatalln(err.Error())
	}

	// web views
	//fs := http.FileServer(http.Dir(root + string(os.PathSeparator) + "www"))
	//http.Handle("/", http.StripPrefix("/", fs))
	indexHTML := mewn.Bytes("./www/index.html")
	previewHTML := mewn.Bytes("./www/preview.html")
	jqueryJs := mewn.Bytes("./www/static/jquery.min.js")
	jsmpegJs := mewn.Bytes("./www/static/jsmpeg.min.js")
	vueJs := mewn.Bytes("./www/static/vue.min.js")
	styleCSS := mewn.Bytes("./www/static/style.css")
	favicon := mewn.Bytes("./www/favicon.ico")
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/" || strings.HasPrefix(r.RequestURI, "/index.html") {
			w.Header().Set("Content-Type", "text/html")
			w.Write(indexHTML)
		} else if strings.HasPrefix(r.RequestURI, "/preview.html") {
			w.Header().Set("Content-Type", "text/html")
			w.Write(previewHTML)
		} else if strings.HasPrefix(r.RequestURI, "/static/jquery.min.js") {
			w.Header().Set("Content-Type", "text/javascript")
			w.Write(jqueryJs)
		} else if strings.HasPrefix(r.RequestURI, "/static/jsmpeg.min.js") {
			w.Header().Set("Content-Type", "text/javascript")
			w.Write(jsmpegJs)
		} else if strings.HasPrefix(r.RequestURI, "/static/vue.min.js") {
			w.Header().Set("Content-Type", "text/javascript")
			w.Write(vueJs)
		} else if strings.HasPrefix(r.RequestURI, "/static/style.css") {
			w.Header().Set("Content-Type", "text/css")
			w.Write(styleCSS)
		} else if strings.HasPrefix(r.RequestURI, "/favicon.ico") {
			w.Header().Set("Content-Type", "image/x-icon")
			w.Write(favicon)
		}
	})

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
		log.Println(uri)
		key := r.URL.Query().Get("key")
		if key == "" {
			log.Println("err stream key")
			return
		}
		s := streamerMap[key]
		if s == nil {
			log.Println("err stream key")
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println(err)
			return
		}

		playerKey := conn.RemoteAddr().String()
		log.Println("player connected: " + playerKey + " -> streamer " + s.Key)
		s.PlayerMap[playerKey] = &player{
			Key:  playerKey,
			Time: time.Now(),
			Conn: conn,
		}

		if s.Lazy {
			s.tryStart()
		}

		go func(conn *websocket.Conn) {
			for {
				_, _, err := conn.ReadMessage()
				if err != nil {
					conn.Close()
					break
				}
			}
			log.Println("conn closed: " + playerKey)
			playerKey := conn.RemoteAddr().String()
			for _, s := range streamerMap {
				if s != nil {
					delete(s.PlayerMap, playerKey)
					if s.Lazy {
						if len(s.PlayerMap) == 0 && s.Alive {
							s.stop()
							log.Println("streamer " + s.Key + " has no player, stop")
						}
					}
				}
			}
		}(conn)
	})

	http.HandleFunc("/streamer/add", func(w http.ResponseWriter, r *http.Request) {
		var key string
		var source string
		var resolution string
		var lazyStr string
		if r.Method == "GET" {
			q := r.URL.Query()
			key = q.Get("key")
			source = q.Get("source")
			resolution = q.Get("resolution")
			lazyStr = q.Get("lazy")
		} else {
			key = r.FormValue("key")
			source = r.FormValue("source")
			resolution = r.FormValue("resolution")
			lazyStr = r.FormValue("lazy")
		}
		key = strings.TrimSpace(key)
		source = strings.TrimSpace(source)
		resolution = strings.TrimSpace(resolution)
		lazy, err := strconv.ParseBool(lazyStr)
		if err != nil {
			log.Println(err)
			lazy = true
		}
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

		stmt, err := db.Prepare("insert into streamer(key, source, resolution, lazy) values(?,?,?,?)")
		if err != nil {
			w.Write([]byte(err.Error()))
			return
		}
		defer stmt.Close()
		_, err = stmt.Exec(key, source, resolution, lazy)
		if err != nil {
			w.Write([]byte(err.Error()))
			return
		}

		s = &streamer{
			Key:        key,
			Source:     source,
			Resolution: resolution,
			Lazy:       lazy,
			Alive:      false,
			Cmd:        nil,
			PlayerMap:  make(map[string]*player),
		}

		if !s.Lazy {
			s.tryStart()
		}

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
				Lazy:       s.Lazy,
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

	go func() {
		for {
			for _, s := range streamerMap {
				if s != nil && !s.Lazy && !s.Alive {
					log.Println("!lazy streamer " + s.Key + " dropped, try start")
					s.tryStart()
				}
			}
			time.Sleep(time.Second * 10)
		}
	}()

	http.ListenAndServe("0.0.0.0:"+strconv.Itoa(port), nil)

}

type streamerModel struct {
	Key        string    `json:"key"`
	Source     string    `json:"source"`
	Resolution string    `json:"resolution"`
	Lazy       bool      `json:"lazy"`
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
