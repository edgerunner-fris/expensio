// Package httpx 定义统一的 HTTP 响应体与分页约定。
//
// 响应体固定为 {code, message, request_id, data, details}：
// 前端拦截器只看 code，不看 HTTP 状态码，也不匹配文案。
package httpx

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/edgerunner-fris/expensio/internal/platform/apperr"
)

const (
	// RequestIDKey 是 gin.Context 中请求 ID 的键。
	RequestIDKey = "request_id"
	// CodeOK 是成功响应的固定业务码。
	CodeOK = "ok"
)

// Envelope 是所有 JSON 响应的外层结构。
type Envelope struct {
	Code      string         `json:"code"`
	Message   string         `json:"message,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Data      any            `json:"data,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// RequestID 返回当前请求的 ID，未经过 requestid 中间件时为空串。
func RequestID(c *gin.Context) string {
	return c.GetString(RequestIDKey)
}

func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{Code: CodeOK, RequestID: RequestID(c), Data: data})
}

func Created(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, Envelope{Code: CodeOK, RequestID: RequestID(c), Data: data})
}

func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

// Error 把错误写入响应体，并挂到 c.Errors 上交给访问日志统一落盘。
// 5xx 只返回通用文案，根因不会出现在响应里。
func Error(c *gin.Context, err error) {
	_ = c.Error(err)
	c.AbortWithStatusJSON(apperr.Status(err), Envelope{
		Code:      apperr.Code(err),
		Message:   apperr.Message(err),
		RequestID: RequestID(c),
		Details:   apperr.Details(err),
	})
}
