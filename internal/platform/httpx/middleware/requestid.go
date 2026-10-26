// Package middleware 提供与业务无关的 HTTP 中间件。
package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
)

const (
	headerRequestID = "X-Request-ID"
	maxRequestIDLen = 64
)

// RequestID 沿用上游传入的合法请求 ID（便于跨服务串联），否则生成新的。
// 非法输入一律丢弃：请求 ID 会进日志，不能成为日志注入的入口。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(headerRequestID)
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		c.Set(httpx.RequestIDKey, id)
		c.Header(headerRequestID, id)
		c.Next()
	}
}

func validRequestID(s string) bool {
	if s == "" || len(s) > maxRequestIDLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
