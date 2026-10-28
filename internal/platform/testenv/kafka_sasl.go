package testenv

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
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
	"github.com/twmb/franz-go/pkg/sasl/plain"
)

// SASL 测试 broker 的用户名与口令只在容器生命周期内有效。
// 与 PostgreSQL 的测试口令一样，它们不会出现在任何配置文件或部署清单里。
const (
	kafkaSASLUser     = "admin"
	kafkaSASLPassword = "test_sasl_password"
	// SASL 监听端口固定，宿主端口在启动前随机分配。
	kafkaSASLPort = "29094/tcp"
)

// KafkaSASL 是启用 SASL/PLAIN + TLS 的单节点 broker。
//
// 用它验证的是"客户端真的能过 TLS 握手与 SASL 认证"，而不只是选项拼装正确。
// 镜像与明文 Kafka 测试相同（apache/kafka），避免"测的镜像不是要跑的那个"。
type KafkaSASL struct {
	// Brokers 是宿主进程可用的 bootstrap 地址（SASL_SSL）。
	Brokers []string
	// CAFile 是宿主上的 CA 证书路径，供客户端校验 broker。
	CAFile   string
	Username string
	Password string

	dir       string
	container testcontainers.Container
}

var (
	kafkaSASLOnce     sync.Once
	kafkaSASLInstance *KafkaSASL
	kafkaSASLErr      error
)

// StartKafkaSASL 启动（或复用）SASL_SSL 测试 broker。
func StartKafkaSASL(t *testing.T) *KafkaSASL {
	t.Helper()
	requireDocker(t)

	kafkaSASLOnce.Do(func() { kafkaSASLInstance, kafkaSASLErr = startKafkaSASL() })
	if kafkaSASLErr != nil {
		t.Fatalf("testenv: 启动 SASL Kafka 失败: %v", kafkaSASLErr)
	}
	return kafkaSASLInstance
}

func startKafkaSASL() (*KafkaSASL, error) {
	hostPort, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("分配宿主端口失败: %w", err)
	}
	advertised := net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))

	dir, err := os.MkdirTemp("", "expensio-kafka-sasl-")
	if err != nil {
		return nil, fmt.Errorf("创建证书目录失败: %w", err)
	}
	// 容器里的 appuser(uid 1000) 要能读；目录只在本机测试期间存在。
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, fmt.Errorf("调整证书目录权限失败: %w", err)
	}

	caFile, err := writeTestCertificates(dir)
	if err != nil {
		return nil, err
	}

	properties := fmt.Sprintf(`process.roles=broker,controller
node.id=1
controller.quorum.voters=1@localhost:9093
listeners=PLAINTEXT://:9092,CONTROLLER://:9093,SASL_SSL://:29094
advertised.listeners=PLAINTEXT://localhost:9092,SASL_SSL://%s
listener.security.protocol.map=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT,SASL_SSL:SASL_SSL
controller.listener.names=CONTROLLER
inter.broker.listener.name=PLAINTEXT
sasl.enabled.mechanisms=PLAIN
listener.name.sasl_ssl.plain.sasl.jaas.config=org.apache.kafka.common.security.plain.PlainLoginModule required user_%s="%s";
ssl.keystore.type=PEM
ssl.keystore.location=/etc/kafka/secrets/server.pem
ssl.truststore.type=PEM
ssl.truststore.location=/etc/kafka/secrets/ca.pem
log.dirs=/tmp/kraft-combined-logs
offsets.topic.replication.factor=1
transaction.state.log.replication.factor=1
transaction.state.log.min.isr=1
group.initial.rebalance.delay.ms=0
num.partitions=6
auto.create.topics.enable=false
`, advertised, kafkaSASLUser, kafkaSASLPassword)
	if err := os.WriteFile(filepath.Join(dir, "server.properties"), []byte(properties), 0o644); err != nil {
		return nil, fmt.Errorf("写 server.properties 失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), containerStartTime)
	defer cancel()

	// 不用镜像自带的 configure 脚本：它为 SSL 监听器强制要求 keystore 口令文件，
	// 而 Kafka 的 PEM keystore 不允许设置 store 口令。直接给它一份 server.properties
	// 更直接，也少一层镜像脚本行为要跟着升级。
	startScript := "/opt/kafka/bin/kafka-storage.sh format --ignore-formatted " +
		"-t 5L6g3nShT-eMCtK--X86sw -c /etc/kafka/secrets/server.properties && " +
		"exec /opt/kafka/bin/kafka-server-start.sh /etc/kafka/secrets/server.properties"

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        kafkaImage,
			ExposedPorts: []string{kafkaSASLPort},
			Entrypoint:   []string{"/bin/bash", "-c"},
			Cmd:          []string{startScript},
			Mounts: testcontainers.ContainerMounts{
				testcontainers.BindMount(dir, "/etc/kafka/secrets"),
			},
			WaitingFor: wait.ForAll(
				wait.ForLog("Kafka Server started"),
				wait.ForListeningPort(kafkaSASLPort),
			).WithStartupTimeout(containerStartTime),
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.PortBindings = network.PortMap{
					network.MustParsePort(kafkaSASLPort): {
						{HostPort: strconv.Itoa(hostPort)},
					},
				}
			},
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("启动 SASL Kafka 容器失败: %w", err)
	}

	return &KafkaSASL{
		Brokers:   []string{advertised},
		CAFile:    caFile,
		Username:  kafkaSASLUser,
		Password:  kafkaSASLPassword,
		dir:       dir,
		container: ctr,
	}, nil
}

// CreateTopic 用 SASL_SSL 建主题。测试客户端与生产客户端走同一套认证。
func (k *KafkaSASL) CreateTopic(t *testing.T, name string, partitions int32) {
	t.Helper()

	caPEM, err := os.ReadFile(k.CAFile)
	if err != nil {
		t.Fatalf("testenv: 读取测试 CA 失败: %v", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("testenv: 测试 CA 不是有效的 PEM")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := kgo.NewClient(
		kgo.SeedBrokers(k.Brokers...),
		kgo.DialTLSConfig(newClientTLSConfig(pool)),
		kgo.SASL(plain.Auth{User: k.Username, Pass: k.Password}.AsMechanism()),
	)
	if err != nil {
		t.Fatalf("testenv: 创建 SASL 客户端失败: %v", err)
	}
	defer client.Close()

	createTopic(t, ctx, client, name, partitions)
}

// createTopic 是明文与 SASL 两种测试 broker 共用的建主题逻辑。
func createTopic(t *testing.T, ctx context.Context, client *kgo.Client, name string, partitions int32) {
	t.Helper()

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

// newClientTLSConfig 与生产客户端保持同样的最低版本要求。
func newClientTLSConfig(pool *x509.CertPool) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
}

// writeTestCertificates 生成一次性 CA 与带 127.0.0.1/localhost SAN 的服务端证书。
func writeTestCertificates(dir string) (string, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("生成 CA 私钥失败: %w", err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "expensio-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return "", fmt.Errorf("签发测试 CA 失败: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return "", fmt.Errorf("解析测试 CA 失败: %w", err)
	}

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("生成服务端私钥失败: %w", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return "", fmt.Errorf("签发服务端证书失败: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return "", fmt.Errorf("编码服务端私钥失败: %w", err)
	}

	caPath := filepath.Join(dir, "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		return "", fmt.Errorf("写 CA 证书失败: %w", err)
	}

	// Kafka 的 PEM keystore 把私钥与证书链放在同一个文件里。
	var serverPEM []byte
	serverPEM = append(serverPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...)
	serverPEM = append(serverPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})...)
	if err := os.WriteFile(filepath.Join(dir, "server.pem"), serverPEM, 0o644); err != nil {
		return "", fmt.Errorf("写服务端证书失败: %w", err)
	}
	return caPath, nil
}
