// Package local 是内置的「本地文件系统」sink。
//
// 它本身不搬运数据——下载引擎已经把章节图片落到本地目录了；
// 它的存在是为了让 sink 注册表非空、让「云盘上传」这种未来插件
// 有一条可参照的最小实现样本。配置只需要一个 data_dir。
package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"tsukimi/internal/sink"
)

type Sink struct {
	dataDir string
}

func New() *Sink { return &Sink{} }

func (s *Sink) ID() string          { return "local" }
func (s *Sink) DisplayName() string { return "本地文件系统" }

func (s *Sink) Configure(cfg map[string]any) error {
	if v, ok := cfg["data_dir"].(string); ok && v != "" {
		s.dataDir = v
	}
	return nil
}

func (s *Sink) Test(ctx context.Context) error {
	if s.dataDir == "" {
		return fmt.Errorf("local sink: data_dir 未配置")
	}
	probe := filepath.Join(s.dataDir, ".tsukimi.write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("local sink: 写入 %s 失败: %w", s.dataDir, err)
	}
	return os.Remove(probe)
}

// Upload 把已下载的目录「登记」一次：检查存在性，把本地路径作为 URL 回报。
// 真正的云盘 sink 会在这里做实际的网络上传。
func (s *Sink) Upload(ctx context.Context, job sink.UploadJob) (sink.Result, error) {
	if _, err := os.Stat(job.LocalDir); err != nil {
		return sink.Result{}, fmt.Errorf("本地目录不可用: %w", err)
	}
	return sink.Result{
		OK:     true,
		URL:    "file://" + job.LocalDir,
		Detail: fmt.Sprintf("已就位于本地：%s", job.LocalDir),
	}, nil
}
