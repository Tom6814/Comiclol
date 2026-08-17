package jmcomic

import "time"

const days14 = 14 * 24 * time.Hour

// futureDuration 返回从现在起经过 dur 后的时间点。
func futureDuration(dur time.Duration) time.Time {
	return time.Now().Add(dur)
}
