package library

import "time"

// now 返回当前 UTC 时间，便于测试时替换。
func now() time.Time { return time.Now().UTC() }
