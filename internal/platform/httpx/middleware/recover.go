package middleware

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
	"github.com/edgerunner-fris/expensio/internal/platform/httpx"
)

// Recovery 捕获 panic，记录堆栈，返回统一格式的 500。
//
// http.ErrAbortHandler 必须原样抛出：net/http 用它表示"连接已被放弃"，
// 吞掉它会掩盖真实的客户端断开行为。
func Recovery(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}

			log.Error("panic recovered",
				zap.Any("panic", rec),
				zap.Stack("stack"),
				zap.String("request_id", httpx.RequestID(c)),
				zap.String("path", c.Request.URL.Path),
			)

			httpx.Error(c, apperr.Internal(fmt.Errorf("panic: %v", rec)))
		}()

		c.Next()
	}
}
