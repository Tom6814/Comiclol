package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Config struct {
	mu sync.RWMutex

	Addr        string `json:"addr"`          // HTTP listen address
	DataDir     string `json:"data_dir"`      // root data directory (library + metadata)
	Concurrency int    `json:"concurrency"`   // 单章内并发下载的图片数
	ChapterJobs int    `json:"chapter_jobs"`  // 单个任务内并发下载的章节数（任务级串行，一次只跑一个任务）
	RetryTimes  int    `json:"retry_times"`   // per-request retry count
	ImageQuality int   `json:"image_quality"` // jpeg re-encode quality (1-100), 0 = keep original where possible

	// Sync favorites polling
	SyncEnabled  bool `json:"sync_enabled"`
	SyncInterval int  `json:"sync_interval"` // seconds
	// SyncRecentCount：每次同步最多处理「最近 N 本」收藏，0 表示不限（同步全部）。
	// 远端收藏列表默认按收藏时间倒序，所以前 N 本即「最近 N 本」；
	// 已下载的不计入新增，下载完成后旧的无需再管，后续只增量同步新收藏。
	SyncRecentCount int `json:"sync_recent_count"`

	// JMComic defaults
	JM struct {
		Domains   []string `json:"domains"`
		ImageHost string   `json:"image_host"`
		Username  string   `json:"username"`
		Password  string   `json:"password"`
		AVSCookie string   `json:"avs_cookie"`
	} `json:"jm"`

	// Plugins keyed by id -> arbitrary config
	Plugins map[string]map[string]any `json:"plugins"`

	path string
}

func Default(dataDir string) *Config {
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".tsukimi")
	}
	c := &Config{
		Addr:        ":7878",
		DataDir:     dataDir,
		Concurrency: 8,
		ChapterJobs: 4,
		RetryTimes:  5,
		ImageQuality: 92,
		SyncInterval: 600,
		Plugins:      map[string]map[string]any{},
	}
	c.JM.Domains = []string{"18comic.vip", "18comic.org", "jm-comic.club"}
	c.JM.ImageHost = "cdn-msp2.18comic.org"
	return c
}

func Load(path string) (*Config, error) {
	c := Default(filepath.Dir(path))
	c.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, c.Save()
		}
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, err
	}
	if c.path == "" {
		c.path = path
	}
	return c, nil
}

func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o644)
}

func (c *Config) PluginConfig(id string) map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.Plugins == nil {
		return nil
	}
	return c.Plugins[id]
}

func (c *Config) SetPluginConfig(id string, cfg map[string]any) {
	c.mu.Lock()
	if c.Plugins == nil {
		c.Plugins = map[string]map[string]any{}
	}
	c.Plugins[id] = cfg
	c.mu.Unlock()
	_ = c.Save()
}

func (c *Config) Update(fn func(*Config)) error {
	c.mu.Lock()
	fn(c)
	c.mu.Unlock()
	return c.Save()
}

func (c *Config) EnsureDirs() error {
	c.mu.RLock()
	dirs := []string{
		c.DataDir,
		filepath.Join(c.DataDir, "library"),
		filepath.Join(c.DataDir, "covers"),
		filepath.Join(c.DataDir, "metadata"),
		filepath.Join(c.DataDir, "cache"),
	}
	c.mu.RUnlock()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
