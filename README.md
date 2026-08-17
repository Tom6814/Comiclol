# tsukimi · 月見

一个用 Go 写的个人漫画管理站 / 收藏簿，当前对接 **禁漫天堂（JMComic）**。
所有数据落到本地，附带一个 Morandi 风格的内置阅读器，单一可执行文件部署。

> 「tsukimi」是日文「月見（つきみ）」—— 赏月。给一个安静的、只属于自己的小书库起的名字。

---

## 特性

- **以 APP 接口为主**：默认走禁漫 APP 移动端 API（结构化 JSON，不受 IP 地区限制），HTML 端作为可选兜底
  - AES-256-ECB + MD5 token 签名 + PKCS7 填充
  - 图片去混淆按 `MD5(aid+filename)` 的实际算法实现（阈值 `268850` / `421926`）
- **万物皆插件**：cordis 风格的 `Context + EventBus + 服务注册`，扩展点分三类
  - `source.Source` —— 漫画来源（禁漫是内置之一，未来可接更多）
  - `sink.Sink` —— 下载产物去向（内置本地 FS，云盘上传预留接口）
  - `hook` —— 事件钩子（下载生命周期、同步生命周期等）
- **下载引擎**：章节并发 + 章节内图片并发的两级并发；断点续传基于「目标文件已存在且大小>0 则跳过」，无需额外状态文件
- **收藏同步**：定时轮询远端收藏夹，发现本地没有的漫画自动入队下载
- **单一二进制**：前端用 `go:embed` 打进二进制，最终产物只有一个可执行文件
- **Morandi 前端**：暖奶白 + 灰玫瑰 + 鼠尾草绿。**刻意避开**玻璃磨砂、Material Design 3、蓝紫渐变那套「AI 味」的视觉语言

---

## 快速开始

### 依赖

- Go 1.22+（用了 Go 1.22 的新 `ServeMux` 路由语法 `{id}`）
- 一个能访问禁漫的网络环境

### 构建并运行

```bash
git clone https://github.com/Tom6814/Comiclol.git
cd Comiclol
go build -o tsukimi .

# 默认数据目录是 $HOME/.tsukimi，默认监听 :7878
./tsukimi
```

打开浏览器访问 `http://localhost:7878`。

### 命令行参数

```
-config string   配置文件路径（默认 ./config.json，不存在会自动生成）
-data   string   数据目录（默认 $HOME/.tsukimi，覆盖配置）
-addr   string   HTTP 监听地址（默认 :7878，覆盖配置）
```

示例：

```bash
./tsukimi -data /var/tsukimi -addr 0.0.0.0:8787
```

---

## 配置

首次运行会在工作目录生成 `config.json`：

```json
{
  "addr":          ":7878",
  "data_dir":      "$HOME/.tsukimi",
  "concurrency":   8,
  "chapter_jobs":  2,
  "retry_times":   5,
  "image_quality": 92,
  "sync_enabled":  false,
  "sync_interval": 600,
  "jm": {
    "domains":    ["18comic.vip", "18comic.org", "jm-comic.club"],
    "image_host": "cdn-msp2.18comic.org",
    "username":   "",
    "password":   "",
    "avs_cookie": ""
  },
  "plugins": {
    "jmcomic": { "impl": "api" }
  }
}
```

| 字段 | 说明 |
| --- | --- |
| `concurrency` | 单章节内图片并发数 |
| `chapter_jobs` | 同一部漫画内章节并发数 |
| `retry_times` | 单个 HTTP 请求的重试次数 |
| `image_quality` | JPEG 重编码质量（1-100） |
| `sync_enabled` / `sync_interval` | 收藏自动同步开关与轮询间隔（秒） |
| `jm.username` / `jm.password` | 禁漫账号；登录后才能拉收藏 |
| `jm.avs_cookie` | 已知 AVS cookie 时可免登录（格式 `AVS=xxx; ORI=yyy`） |
| `plugins.jmcomic.impl` | 传输实现，`api`（默认）或 `html` |

> 账号、cookie 也可以在前端「设置」页填写，会落回 `config.json`。
> 这些字段包含敏感信息，`config.json` 已在 `.gitignore` 中。

---

## 数据目录布局

```
$DATA_DIR/
├── library.json        # 书库元数据（所有已入库漫画）
├── library/            # 漫画章节图片
│   └── jmcomic_<id>/
│       └── <chapter_id>/
│           ├── 0001.jpg
│           └── ...
└── covers/             # 封面缓存
    └── jmcomic_<id>.jpg
```

`config.json` 与二进制本身放在启动时的工作目录，与数据目录分开。

---

## HTTP API

所有接口返回 JSON，SSE 端点除外。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/health` | 健康检查 |
| GET | `/api/sources` | 已注册的来源列表 |
| GET | `/api/sinks` | 已注册的 sink 列表 |
| GET | `/api/plugins` | 插件总览 |
| GET | `/api/config` | 读取配置（敏感字段也会返回） |
| PUT | `/api/config` | 更新配置 |
| GET | `/api/session` | 当前登录态 |
| POST | `/api/login` | 登录某来源 |
| POST | `/api/logout` | 注销某来源 |
| GET | `/api/library` | 书库列表 |
| GET | `/api/library/{source}/{id}` | 漫画详情（本地无则向远端拉） |
| DELETE | `/api/library/{source}/{id}?files=1` | 从书库移除，`files=1` 同时删本地文件 |
| GET | `/api/library/{source}/{id}/cover` | 封面（本地无则 302 到远端） |
| GET | `/api/library/{source}/{id}/{chapter}/pages` | 该章节已下载的图片列表 |
| GET | `/api/library/{source}/{id}/{chapter}/{file}` | 单张图片（带长缓存） |
| GET | `/api/favorites?source=jmcomic&page=N` | 远端收藏分页 |
| POST | `/api/favorites/sync` | 手动触发一次收藏同步 |
| POST | `/api/downloads` | 提交一个下载任务 |
| GET | `/api/downloads` | 下载任务列表 |
| POST | `/api/downloads/{id}/cancel` | 取消任务 |
| DELETE | `/api/downloads/{id}` | 删除任务记录 |
| GET  | `/api/events` | **SSE**：实时事件流（进度、完成、同步发现新条目等） |
| GET | `/` | 内置前端（单文件 HTML） |

### 提交下载示例

```bash
curl -X POST http://localhost:7878/api/downloads \
  -H 'Content-Type: application/json' \
  -d '{"source":"jmcomic","manga_id":"123456","title":"示例"}'
# {"task_id":"..."}
```

### 监听实时进度示例

```bash
curl -N http://localhost:7878/api/events
# event: hello
# data: {"ok":true}
# event: download.progress
# data: {"type":"download.progress","payload":{"task_id":"...","done":3,"total":24,"progress":0.125}}
```

---

## 项目结构

```
tsukimi/
├── main.go                      # 入口，按依赖顺序装配所有服务
├── internal/
│   ├── config/                  # JSON 配置（带锁）
│   ├── domain/                  # 来源无关的领域模型
│   ├── plugin/                  # cordis 风格插件系统（Context/EventBus/Logger）
│   ├── hook/                    # 事件主题常量
│   ├── source/                  # Source 接口 + Registry
│   ├── sink/                    # Sink 接口 + Registry
│   │   └── local/               # 内置本地 FS sink
│   ├── jmcomic/                 # 禁漫源插件
│   │   ├── jmcomic.go           #   插件主体，实现 source.Source
│   │   ├── api_client.go        #   APP API 实现（默认）
│   │   ├── api_parse.go         #   APP API JSON 解析
│   │   ├── html_client.go       #   HTML 实现（兜底）
│   │   ├── parse.go             #   HTML 正则解析
│   │   ├── crypto.go            #   token 签名 + AES-ECB 加解密
│   │   └── util.go
│   ├── img/                     # 图片去混淆 + 格式处理
│   ├── store/                   # 文件原子 JSON 存储
│   ├── library/                 # 本地书库服务（元数据 + 文件布局）
│   ├── session/                 # 会话管理（内存态）
│   ├── download/                # 下载引擎（两级并发 + 断点续传）
│   ├── syncfav/                 # 收藏自动同步
│   ├── httpclient/              # 通用 HTTP 客户端
│   └── server/                  # HTTP 层 + SSE + 内嵌前端
│       ├── server.go
│       ├── sse.go
│       ├── util.go
│       └── static/index.html    # Morandi 单文件前端
└── go.mod
```

---

## 测试

```bash
go test ./...
```

当前覆盖：
- `internal/img` —— 图片去混淆算法的 `ComputeNum`、纵向条带还原、scramble_id 解析
- `internal/jmcomic` —— token 签名、AES-ECB 往返、PKCS7 填充、固定时间戳三元组缓存

---

## 扩展开发

### 加一个新的漫画来源

实现 `source.Source` 接口，然后调用 `source.Registry.Register(yourSource)`：

```go
type Source interface {
    ID() string
    DisplayName() string
    Login(ctx context.Context, creds domain.Credentials) (domain.Session, error)
    GetManga(ctx context.Context, sess domain.Session, mangaID string) (*domain.Manga, error)
    GetChapter(ctx context.Context, sess domain.Session, chapterID string) (*domain.Chapter, []domain.Page, error)
    Favorites(ctx context.Context, sess domain.Session, folderID string, page int) (*domain.FavoritePage, error)
    FavoriteFolders(ctx context.Context, sess domain.Session) ([]domain.Folder, error)
    FetchImage(ctx context.Context, sess domain.Session, page domain.Page) (domain.ImageData, error)
    Capabilities() Capabilities
}
```

可选实现 `source.Searcher` 获得 `/api/.../search` 能力。

### 加一个云盘上传 sink

实现 `sink.Sink` 接口：

```go
type Sink interface {
    ID() string
    DisplayName() string
    Configure(cfg map[string]any) error
    Upload(ctx context.Context, job UploadJob) (Result, error)
    Test(ctx context.Context) error
}
```

`UploadJob.LocalDir` 是已下载好的章节目录；把里面文件上传到你自己的云盘，返回 `Result{URL: ...}` 即可。本地 FS 是参考实现。

### 订阅事件

```go
ctx.Bus.On(hook.DownloadComplete, func(ctx context.Context, ev plugin.Event) error {
    // ev.Payload 里有 task_id / manga_id / path 等
    return nil
})
```

可用主题见 `internal/hook/topics.go`。

---

## 已知限制

- 同步服务目前按「本地元数据是否存在 + 是否已下载」判断是否需要同步；
  「远端某部已入库漫画新增了章节」这种增量更新暂未实现，需要手动重新下载。
- `httpclient` 包是为早期单传输实现写的；切换到双 client 后 `api_client` / `html_client`
  各自持有 HTTP 客户端，旧包暂保留但未在主路径使用。
- 图片去混淆基于禁漫当前的算法实现，若上游再次调整阈值或公式需要同步更新 `internal/img/decode.go`。

---

## License

见仓库根目录 `LICENSE`。
