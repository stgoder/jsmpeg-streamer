# jsmpeg-streamer

#### 介绍
jsmpeg rtsp|rtmp|file... h5播放，ffmpeg转码进程启停管理，进程按需启动  

#### 编译

1.  mewn把web文件一起打包binary  
    `.\mewn.exe build -ldflags="-w -s"`  
    `.\mewn.exe build -ldflags="-w -s -H windowsgui"` 隐藏窗口
2.  upx压缩binary大小 (可选)  
    `.\upx.exe -9 .\jsmpeg-streamer.exe`

#### 使用

- 运行可选参数，-p [端口] 默认10019，-ff [ffmpeg路径] 默认使用同级路径，其次使用环境变量
- web页面，http://[ip]:[port]
- http api
  - /streamer/add  
    key: string (\*) 唯一标识  
    source: string (\*) 源地址，rtsp|rtmp|file...  
    resolution: string 分辨率，默认原始分辨率  
    lazy: bool 是否只在有播放时启动，默认true
  - /streamer/delete
    key: string (\*) 唯一标识
  - /streamer/list
- 页面引入 /static/jsmpeg.min.js，ws://[ip]:[port]/relay?key=1  
    ````
    <canvas id="canvas0"></canvas>
    <script>
    player0 = new JSMpeg.Player('ws://localhost:10019/relay?key=1', {
        canvas: document.getElementById('canvas0')
    })
    </script>
    ````
