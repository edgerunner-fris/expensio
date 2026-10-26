package testenv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

const (
	kafkaImage = "apache/kafka:3.9.2"
	// 容器内 broker 监听端口。宿主端口在启动前随机分配，见 startKafka。
	kafkaBrokerPort = "29092/tcp"
)

// Kafka 是单节点 KRaft 模式的测试 broker。
type Kafka struct {
	// Brokers 是宿主进程可用的 bootstrap 地址。
	Brokers []string

	container testcontainers.Container
}

var (
	kafkaOnce     sync.Once
	kafkaInstance *Kafka
	kafkaErr      error
)

// StartKafka 启动（或复用）测试用 Kafka，返回宿主可达的 bootstrap 地址。
//
// 这里没有用官方 testcontainers Kafka 模块：该模块只支持
// confluentinc/confluent-local，而本项目本地与生产统一使用 apache/kafka。
// 用两个不同的镜像测，等于没有验证真正会运行的那个。
func StartKafka(t *testing.T) *Kafka {
	t.Helper()
	requireDocker(t)

	kafkaOnce.Do(func() { kafkaInstance, kafkaErr = startKafka() })
	if kafkaErr != nil {
		t.Fatalf("testenv: 启动 Kafka 失败: %v", kafkaErr)
	}
	return kafkaInstance
}

func startKafka() (*Kafka, error) {
	// 必须在启动前确定宿主机端口：advertised listener 一旦写进 broker 配置，
	// 就无法在容器启动后再改。先占一个空闲端口，再显式绑定到宿主机。
	hostPort, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("分配宿主端口失败: %w", err)
	}
	advertised := net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))

	ctx, cancel := context.WithTimeout(context.Background(), containerStartTime)
	defer cancel()

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        kafkaImage,
			ExposedPorts: []string{kafkaBrokerPort},
			Env: map[string]string{
				"KAFKA_NODE_ID":                                  "1",
				"KAFKA_PROCESS_ROLES":                            "broker,controller",
				"KAFKA_CONTROLLER_QUORUM_VOTERS":                 "1@localhost:9093",
				"KAFKA_LISTENERS":                                "PLAINTEXT://:9092,CONTROLLER://:9093,PLAINTEXT_HOST://:29092",
				"KAFKA_ADVERTISED_LISTENERS":                     "PLAINTEXT://localhost:9092,PLAINTEXT_HOST://" + advertised,
				"KAFKA_LISTENER_SECURITY_PROTOCOL_MAP":           "CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT,PLAINTEXT_HOST:PLAINTEXT",
				"KAFKA_CONTROLLER_LISTENER_NAMES":                "CONTROLLER",
				"KAFKA_INTER_BROKER_LISTENER_NAME":               "PLAINTEXT",
				"KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR":         "1",
				"KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR": "1",
				"KAFKA_TRANSACTION_STATE_LOG_MIN_ISR":            "1",
				"KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS":         "0",
				"KAFKA_NUM_PARTITIONS":                           "6",
				"KAFKA_AUTO_CREATE_TOPICS_ENABLE":                "false",
			},
			WaitingFor: wait.ForAll(
				wait.ForLog("Kafka Server started"),
				wait.ForListeningPort(kafkaBrokerPort),
			).WithStartupTimeout(containerStartTime),
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.PortBindings = network.PortMap{
					network.MustParsePort(kafkaBrokerPort): {
						{HostPort: strconv.Itoa(hostPort)},
					},
				}
			},
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("启动 Kafka 容器失败: %w", err)
	}
	return &Kafka{Brokers: []string{advertised}, container: ctr}, nil
}

// CreateTopic 显式建主题。
//
// 测试不依赖 auto-create：主题是否已存在、有几个分区，直接决定消费行为；
// 让 broker 在第一次 produce 时"顺手"建主题，会让测试时序变得不可推理。
func (k *Kafka) CreateTopic(t *testing.T, name string, partitions int32) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := kgo.NewClient(kgo.SeedBrokers(k.Brokers...))
	if err != nil {
		t.Fatalf("testenv: 创建 Kafka 客户端失败: %v", err)
	}
	defer client.Close()

	req := kmsg.NewPtrCreateTopicsRequest()
	req.TimeoutMillis = 10000
	topic := kmsg.NewCreateTopicsRequestTopic()
	topic.Topic = name
	topic.NumPartitions = partitions
	topic.ReplicationFactor = 1
	req.Topics = append(req.Topics, topic)

	raw, err := client.Request(ctx, req)
	if err != nil {
		t.Fatalf("testenv: 建主题 %s 失败: %v", name, err)
	}
	resp, ok := raw.(*kmsg.CreateTopicsResponse)
	if !ok {
		t.Fatalf("testenv: 建主题返回了意外类型 %T", raw)
	}
	for _, rt := range resp.Topics {
		if rt.Topic != name {
			continue
		}
		// TOPIC_ALREADY_EXISTS 视为成功：同一容器的多个测试可能复用主题。
		if rt.ErrorCode != 0 && rt.ErrorCode != 36 {
			message := ""
			if rt.ErrorMessage != nil {
				message = *rt.ErrorMessage
			}
			t.Fatalf("testenv: 建主题 %s 失败: code=%d message=%s",
				name, rt.ErrorCode, message)
		}
		return
	}
	t.Fatalf("testenv: 建主题 %s 的响应里没有该主题", name)
}

// freePort 让内核分配一个空闲的 TCP 端口，随即释放。
// 到容器启动之间理论上存在竞争，但端口范围由内核轮转，测试环境里足够可靠；
// 万一被抢占，Docker 会明确报端口占用，而不是静默连错 broker。
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("testenv: 无法解析监听地址")
	}
	return addr.Port, nil
}
