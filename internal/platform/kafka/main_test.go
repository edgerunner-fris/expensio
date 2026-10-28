package kafka_test

import (
	"os"
	"testing"

	"github.com/edgerunner-fris/expensio/internal/platform/testenv"
)

// TestMain 负责回收本包启动的 Kafka 容器（明文与 SASL 两个）。
// 没有它，容器会残留到下一次手工清理——Ryuk 在 testenv 里是显式关闭的。
func TestMain(m *testing.M) {
	code := m.Run()
	testenv.Cleanup()
	os.Exit(code)
}
