package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
)

// retryTransport 只对幂等请求、且错误明确发生在"请求送达之前"的情况下重试一次。
//
// 三条界线：
//   - 非幂等方法不重试：一次 POST 被送达两次就是重复提交。
//   - 请求体非空不重试：body 在读了一次之后无法确定还能不能重放。
//   - 超时不重试：连接已建立、请求可能已经到达下游，重试只会重复执行。
//
// 重试是立即进行的，不用 sleep 排队：连不上就是连不上，等待只会积累连接。
type retryTransport struct {
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil || !retryable(req) || !connectionFailed(err) {
		return resp, err
	}
	// 同一个请求对象不能被并发复用，但这里是顺序重试，且 body 为空（见 retryable）。
	return t.base.RoundTrip(req)
}

func retryable(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	return req.Body == nil || req.Body == http.NoBody
}

// connectionFailed 判定"请求肯定没有送达"的错误：
// 连接被拒绝、DNS 失败、连接在写出请求前被重置。
func connectionFailed(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		// 写请求失败才会走到 syscall 分支；读失败（等响应）说明请求已经送达。
		return errors.Is(netErr.Err, syscall.ECONNREFUSED) ||
			errors.Is(netErr.Err, syscall.ECONNRESET) ||
			errors.Is(netErr.Err, syscall.EHOSTUNREACH) ||
			errors.Is(netErr.Err, syscall.ENETUNREACH)
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}
