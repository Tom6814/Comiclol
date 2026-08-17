package server

import "time"

// timeTick 暴露 ticker 工厂，便于将来测试替换。
func timeTick() *time.Ticker {
	return time.NewTicker(15 * time.Second)
}
