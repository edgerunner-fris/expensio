package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 关闭钩子的顺序与"只执行一次"都是生命周期契约：
// 连接池必须在最后释放（HTTP 已经停了才关），而 setup 失败与正常退出的
// 两条路径都可能触发 close，重复执行会把已关闭的池再关一次。
func TestCloseRunsHooksInReverseOrderExactlyOnce(t *testing.T) {
	var got []string
	app := &App{}
	app.OnStop(func() { got = append(got, "http") })
	app.OnStop(func() { got = append(got, "db") })
	app.OnStop(nil) // 空钩子被忽略，不能把 nil 塞进调用链

	app.close()
	app.close()

	assert.Equal(t, []string{"db", "http"}, got)
}
