package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"

	"github.com/edgerunner-fris/expensio/internal/platform/telemetry"
)

// 熔断器的两类观测点分开计数：
//   - transitions 记录 closed → open → half-open 的状态切换，用于确认"熔断真的发生过"；
//   - rejections 记录打开期间被直接挡回的请求数，用于估算故障期间省下的下游压力。
var (
	circuitTransitions = telemetry.NewCounter(
		"expensio.gateway.circuit.transitions",
		"网关熔断器状态切换次数（按下游与起止状态）",
		"{transition}",
	)
	circuitRejections = telemetry.NewCounter(
		"expensio.gateway.circuit.rejections",
		"熔断打开期间被网关直接拒绝的请求数",
		"{request}",
	)
)

// errCircuitOpen 是熔断器打开（或半开探测额度已满）时返回到 ReverseProxy 的哨兵错误。
// ErrorHandler 靠它把响应从 502 改成 503 + Retry-After：502 表示"下游答错了"，
// 503 表示"我们主动不转发"，两者的告警含义完全不同。
var errCircuitOpen = errors.New("gateway: 下游熔断器打开")

// errUpstream5xx 只用于让熔断器把 502/503/504 计为失败。
// 它不会返回给调用方：响应本身会原样转发（见 circuitTransport.RoundTrip）。
var errUpstream5xx = errors.New("gateway: 下游返回 5xx")

const (
	defaultCircuitFailures    = 5
	defaultCircuitTimeout     = 30 * time.Second
	defaultCircuitMaxRequests = 1
)

// CircuitOptions 是每个下游独立熔断器的参数。
//
// 零值全部取默认值，因此现有装配代码不传也能启用熔断。
// 每个下游各有一个实例：报表慢查询拖垮 report 时，identity 的登录不应一起被熔断。
type CircuitOptions struct {
	// Failures 是连续失败多少次后打开熔断器。
	Failures int
	// Interval 是 closed 状态下计数窗口；0 表示窗口内只增不减，
	// 连续失败的中断由"连续"语义本身保证（一次成功就清零）。
	Interval time.Duration
	// Timeout 是 open 状态持续时间，到期后进入 half-open 放一个探测请求。
	Timeout time.Duration
	// MaxRequests 是 half-open 状态下允许同时在途的探测请求数。
	MaxRequests uint32
}

// withDefaults 填充零值并拒绝无意义的负数。
func (o CircuitOptions) withDefaults() (CircuitOptions, error) {
	if o.Failures < 0 || o.Interval < 0 || o.Timeout < 0 {
		return CircuitOptions{}, errors.New("gateway: 熔断器参数不能为负")
	}
	if o.Failures == 0 {
		o.Failures = defaultCircuitFailures
	}
	if o.Timeout == 0 {
		o.Timeout = defaultCircuitTimeout
	}
	if o.MaxRequests == 0 {
		o.MaxRequests = defaultCircuitMaxRequests
	}
	return o, nil
}

// circuitTransport 把一个下游的 RoundTrip 包进熔断器。
//
// 失败判定：网络错误，以及下游返回 502/503/504。
// 500 不计入——500 往往由个别请求触发，用它熔断会让一次坏输入
// 放大成"整个下游不可用"。客户端取消（context.Canceled）也不计入：
// 那是调用方走了，不是下游坏了。
type circuitTransport struct {
	upstream string
	next     http.RoundTripper
	breaker  *gobreaker.CircuitBreaker[*http.Response]
}

func newCircuitTransport(upstream string, next http.RoundTripper, opts CircuitOptions, logg *zap.Logger) (*circuitTransport, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}

	t := &circuitTransport{upstream: upstream, next: next}
	t.breaker = gobreaker.NewCircuitBreaker[*http.Response](gobreaker.Settings{
		Name:        upstream,
		MaxRequests: opts.MaxRequests,
		Interval:    opts.Interval,
		Timeout:     opts.Timeout,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			return c.ConsecutiveFailures >= uint32(opts.Failures)
		},
		IsExcluded: func(err error) bool {
			return errors.Is(err, context.Canceled)
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			// 状态切换不携带请求上下文（可能由半开探测或定时器触发），
			// 用 Background 只为了给指标一个 ctx；计数器本身不依赖它。
			circuitTransitions.Add(context.Background(), 1,
				attribute.String("upstream", name),
				attribute.String("from", from.String()),
				attribute.String("to", to.String()),
			)
			logg.Warn("gateway: 下游熔断器状态变化",
				zap.String("upstream", name),
				zap.String("from", from.String()),
				zap.String("to", to.String()),
			)
		},
	})
	return t, nil
}

func (t *circuitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// resp 由 Execute 内的闭包同步写入，随后的分支在同一 goroutine 读取。
	// 这样做的原因：gobreaker 只按 error 判定成败，而 5xx 是需要照常转发的响应，
	// 不能把它变成 error 丢掉。
	var resp *http.Response
	out, err := t.breaker.Execute(func() (*http.Response, error) {
		r, roundErr := t.next.RoundTrip(req)
		if roundErr != nil {
			return nil, roundErr
		}
		resp = r
		switch r.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return nil, errUpstream5xx
		}
		return r, nil
	})

	switch {
	case errors.Is(err, errUpstream5xx) && resp != nil:
		return resp, nil
	case errors.Is(err, gobreaker.ErrOpenState), errors.Is(err, gobreaker.ErrTooManyRequests):
		circuitRejections.Add(req.Context(), 1, attribute.String("upstream", t.upstream))
		return nil, fmt.Errorf("%w: %v", errCircuitOpen, err)
	default:
		return out, err
	}
}
