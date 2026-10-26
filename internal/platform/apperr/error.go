// Package apperr 提供跨服务统一的错误模型。
//
// 三个约束：
//   - 错误携带稳定的业务码（Code），前端据此分支与 i18n，而不是匹配文案；
//   - HTTP 状态码只在这里映射一次，handler 不再各自猜测；
//   - 内部原因存放在 cause 里，只进日志，绝不进响应体。
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// 通用业务码。领域错误使用 "<domain>.<reason>" 形式，例如 expense.state_conflict。
const (
	CodeInvalidArgument = "common.invalid_argument"
	CodeUnauthorized    = "auth.unauthorized"
	CodeForbidden       = "auth.forbidden"
	CodeNotFound        = "common.not_found"
	CodeConflict        = "common.conflict"
	CodeInternal        = "common.internal"
)

// Error 是需要返回给调用方的业务错误。
// 修改类方法都返回副本，避免同一个错误实例被多个调用方并发改写。
type Error struct {
	code    string
	message string
	status  int
	details map[string]any
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.code, e.message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

// Unwrap 让 errors.Is/As 能穿透到根因。
func (e *Error) Unwrap() error { return e.cause }

// Is 支持按业务码匹配，使 errors.Is(err, apperr.NotFound("费用")) 成立。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.code == e.code
}

func (e *Error) Code() string            { return e.code }
func (e *Error) Message() string         { return e.message }
func (e *Error) Status() int             { return e.status }
func (e *Error) Details() map[string]any { return e.details }

// New 构造一个业务错误。code 由调用方保证稳定。
func New(code string, status int, message string) *Error {
	return &Error{code: code, status: status, message: message}
}

func InvalidArgument(message string) *Error {
	return New(CodeInvalidArgument, http.StatusBadRequest, message)
}

func Unauthorized(message string) *Error {
	return New(CodeUnauthorized, http.StatusUnauthorized, message)
}

func Forbidden(message string) *Error {
	return New(CodeForbidden, http.StatusForbidden, message)
}

func NotFound(resource string) *Error {
	return New(CodeNotFound, http.StatusNotFound, resource+"不存在")
}

// Conflict 用于状态机非法转移、唯一约束冲突等场景，code 由调用方给出。
func Conflict(code, message string) *Error {
	return New(code, http.StatusConflict, message)
}

// Internal 包装不可对外暴露的根因，响应体只保留通用文案。
func Internal(cause error) *Error {
	return (&Error{
		code:    CodeInternal,
		status:  http.StatusInternalServerError,
		message: "服务内部错误，请稍后重试",
	}).WithCause(cause)
}

// WithCause 返回带根因的副本。根因只应出现在日志里。
func (e *Error) WithCause(cause error) *Error {
	cp := *e
	cp.cause = cause
	return &cp
}

// WithDetail 返回附带上下文信息的副本，用于响应体 details 字段。
func (e *Error) WithDetail(key string, value any) *Error {
	cp := *e
	cp.details = make(map[string]any, len(e.details)+1)
	for k, v := range e.details {
		cp.details[k] = v
	}
	cp.details[key] = value
	return &cp
}

// As 提取链路上的第一个 *Error。
func As(err error) (*Error, bool) {
	var target *Error
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

// Status 返回应写回的 HTTP 状态码，非业务错误一律 500。
func Status(err error) int {
	if e, ok := As(err); ok {
		return e.status
	}
	return http.StatusInternalServerError
}

func Code(err error) string {
	if e, ok := As(err); ok {
		return e.code
	}
	return CodeInternal
}

// Message 只返回可以安全展示给调用方的文案。
func Message(err error) string {
	if e, ok := As(err); ok {
		return e.message
	}
	return "服务内部错误，请稍后重试"
}

func Details(err error) map[string]any {
	if e, ok := As(err); ok {
		return e.details
	}
	return nil
}

func IsCode(err error, code string) bool {
	e, ok := As(err)
	return ok && e.code == code
}
