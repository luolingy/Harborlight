package dockerx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"harborlight/internal/store"
)

// Manager 封装 Docker Engine API，负责托管实例的完整生命周期。
type Manager struct {
	cli      *client.Client
	dataDir  string
	remote   bool
	endpoint string
}

func New(dataDir string) (*Manager, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("连接 Docker Engine 失败: %w", err)
	}
	return &Manager{cli: cli, dataDir: abs}, nil
}

func NewDevice(device store.Device) (*Manager, error) {
	if !strings.HasPrefix(device.Endpoint, "tcp://") {
		return nil, errors.New("远程设备地址必须使用 tcp://host:2376")
	}
	if !device.TLS {
		return nil, errors.New("远程 Docker Engine 必须启用双向 TLS")
	}
	address := strings.TrimPrefix(device.Endpoint, "tcp://")
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("远程设备地址格式错误: %w", err)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	opts := []client.Opt{client.WithHost(device.Endpoint)}
	if device.TLS {
		if device.CAPath == "" || device.CertPath == "" || device.KeyPath == "" {
			return nil, errors.New("启用 TLS 时必须提供 CA、客户端证书和私钥文件路径")
		}
		opts = append(opts, client.WithTLSClientConfig(device.CAPath, device.CertPath, device.KeyPath))
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("连接 Docker Engine 失败: %w", err)
	}
	return &Manager{cli: cli, dataDir: device.DataDir, remote: true, endpoint: host}, nil
}

func (m *Manager) Close() error { return m.cli.Close() }

// Spec 一键部署的预置规格。
type Spec struct {
	Type      string
	Image     string
	InnerPort int
	Env       func() (map[string]string, error) // 生成凭据
	Notes     func(port int, env map[string]string) string
	Volume    bool // 是否挂载持久化数据卷
}

var specs = map[string]Spec{
	"redis": {
		Type: "redis", Image: "redis:7-alpine", InnerPort: 6379, Volume: true,
		Env: func() (map[string]string, error) {
			pw := "hl-" + uuid.NewString()[:16]
			return map[string]string{"REDIS_PASSWORD": pw}, nil
		},
		Notes: func(p int, e map[string]string) string {
			return fmt.Sprintf("redis-cli -h 127.0.0.1 -p %d -a <REDIS_PASSWORD>  // 容器内连接: redis://:<REDIS_PASSWORD>@localhost:6379", p)
		},
	},
	"postgres": {
		Type: "postgres", Image: "postgres:16-alpine", InnerPort: 5432, Volume: true,
		Env: func() (map[string]string, error) {
			pw := "hl-" + uuid.NewString()[:16]
			return map[string]string{"POSTGRES_PASSWORD": pw, "POSTGRES_USER": "hl", "POSTGRES_DB": "hl"}, nil
		},
		Notes: func(p int, e map[string]string) string {
			return fmt.Sprintf("postgresql://hl:<POSTGRES_PASSWORD>@127.0.0.1:%d/hl", p)
		},
	},
	"sqlite": {
		Type: "sqlite", Image: "coleifer/sqlite-web:latest", InnerPort: 8080, Volume: true,
		Env: func() (map[string]string, error) {
			pw := "hl-" + uuid.NewString()[:16]
			return map[string]string{"SQLITE_DATABASE": "app.db", "PASSWORD": pw}, nil
		},
		Notes: func(p int, e map[string]string) string {
			return fmt.Sprintf("浏览器访问 http://127.0.0.1:%d （密码 SQLITE_PASSWORD 环境变量）", p)
		},
	},
}

// SupportedTypes 返回支持的实例类型。
func SupportedTypes() []string {
	t := make([]string, 0, len(specs)+1)
	for k := range specs {
		t = append(t, k)
	}
	sort.Strings(t)
	return append(t, "custom")
}

// Create 创建并启动一个托管实例。
func (m *Manager) Create(ctx context.Context, inst *store.Instance, customPort int) error {
	env := map[string]string{}
	innerPort := inst.InnerPort
	image := inst.Image

	if inst.Type == "custom" {
		if image == "" {
			return errors.New("自定义实例必须填写镜像")
		}
		if innerPort <= 0 {
			innerPort = customPort
		}
		if innerPort <= 0 {
			return errors.New("自定义实例必须填写容器端口")
		}
	} else {
		spec, ok := specs[inst.Type]
		if !ok {
			return fmt.Errorf("不支持的实例类型: %s", inst.Type)
		}
		image = spec.Image
		innerPort = spec.InnerPort
		var err error
		env, err = spec.Env()
		if err != nil {
			return err
		}
		inst.Image = image
		inst.InnerPort = innerPort
	}

	hostPort := 0
	var err error
	if !m.remote {
		hostPort, err = freePort()
		if err != nil {
			return err
		}
	}
	inst.HostPort = hostPort

	// 拉取镜像
	if err := m.pullImage(ctx, image); err != nil {
		return fmt.Errorf("拉取镜像 %s 失败: %w", image, err)
	}

	envList := make([]string, 0, len(env))
	for k, v := range env {
		envList = append(envList, k+"="+v)
	}
	envJSON, _ := json.Marshal(env)
	inst.EnvJSON = string(envJSON)

	cfg := &container.Config{
		Image:  image,
		Env:    envList,
		Labels: map[string]string{"harborlight.managed": "true", "harborlight.instance": inst.ID},
	}
	if innerPort > 0 {
		port := network.MustParsePort(fmt.Sprintf("%d/tcp", innerPort))
		cfg.ExposedPorts = network.PortSet{port: {}}
	}

	bindingPort := fmt.Sprint(hostPort)
	if m.remote {
		bindingPort = ""
	}
	hc := &container.HostConfig{
		PortBindings: network.PortMap{
			network.MustParsePort(fmt.Sprintf("%d/tcp", innerPort)): []network.PortBinding{
				{HostIP: netip.IPv4Unspecified(), HostPort: bindingPort},
			},
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}
	// 一律持久化：每个实例挂独立数据卷，方便用户留数据
	hc.Binds = []string{m.dataDir + "/" + inst.ID + ":/data"}

	res, err := m.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: cfg, HostConfig: hc, Name: "hl-" + inst.Name + "-" + inst.ID[:8],
	})
	if err != nil {
		return fmt.Errorf("创建容器失败: %w", err)
	}
	inst.ContainerID = res.ID

	if _, err := m.cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = m.cli.ContainerRemove(ctx, res.ID, client.ContainerRemoveOptions{Force: true})
		return fmt.Errorf("启动容器失败: %w", err)
	}
	if m.remote {
		ins, err := m.cli.ContainerInspect(ctx, res.ID, client.ContainerInspectOptions{})
		if err != nil {
			return fmt.Errorf("读取远程端口映射失败: %w", err)
		}
		portKey := network.MustParsePort(fmt.Sprintf("%d/tcp", innerPort))
		bindings := ins.Container.NetworkSettings.Ports[portKey]
		if len(bindings) == 0 {
			return errors.New("远程 Docker Engine 未返回已分配端口")
		}
		if _, err := fmt.Sscanf(bindings[0].HostPort, "%d", &inst.HostPort); err != nil {
			return fmt.Errorf("解析远程主机端口失败: %w", err)
		}
	}

	// 写连接提示
	if inst.Type != "custom" {
		if spec, ok := specs[inst.Type]; ok {
			inst.Notes = spec.Notes(inst.HostPort, env)
			if m.remote {
				inst.Notes = strings.ReplaceAll(inst.Notes, "127.0.0.1", m.endpoint)
			}
		}
	} else {
		host := "127.0.0.1"
		if m.remote {
			host = m.endpoint
		}
		inst.Notes = fmt.Sprintf("http://%s:%d → 容器 :%d", host, hostPort, innerPort)
	}
	inst.Status = "running"
	return nil
}

func (m *Manager) pullImage(ctx context.Context, image string) error {
	// 已存在则跳过
	if _, err := m.cli.ImageInspect(ctx, image); err == nil {
		return nil
	}
	resp, err := m.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer resp.Close()
	return resp.Wait(ctx) // 等待拉取完成
}

// Lifecycle 生命周期操作。
func (m *Manager) Start(ctx context.Context, id string) error {
	_, err := m.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	_, err := m.cli.ContainerStop(ctx, id, client.ContainerStopOptions{})
	return err
}

func (m *Manager) Restart(ctx context.Context, id string) error {
	_, err := m.cli.ContainerRestart(ctx, id, client.ContainerRestartOptions{})
	return err
}

func (m *Manager) Remove(ctx context.Context, id string) error {
	_, err := m.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: false})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// State 同步容器实际状态（不落库，仅展示用）。
func (m *Manager) State(ctx context.Context, id string) string {
	ins, err := m.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return "missing"
		}
		return "unknown"
	}
	st := ins.Container.State
	switch {
	case st.Running:
		return "running"
	case st.Restarting:
		return "restarting"
	case st.OOMKilled:
		return "oom"
	case st.Dead:
		return "dead"
	default:
		return "exited"
	}
}

// Logs 取容器日志尾部。
func (m *Manager) Logs(ctx context.Context, id string, tail int) (string, error) {
	r, err := m.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: fmt.Sprint(tail), Timestamps: true,
	})
	if err != nil {
		return "", err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	// Docker multiplexed stream（TTY 关闭时带 8 字节头部），做一次宽松清洗
	return sanitizeLog(string(b)), nil
}

func sanitizeLog(s string) string {
	// 去除流帧头（0x08 开头 8 字节）带来的乱码行头
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if len(l) > 8 && (l[0] == 1 || l[0] == 2 || l[0] == 6 || l[0] == 8) {
			lines[i] = l[8:]
		}
	}
	return strings.Join(lines, "\n")
}

// Ping 检查 Docker Engine 可达性。
func (m *Manager) Ping(ctx context.Context) error {
	_, err := m.cli.Ping(ctx, client.PingOptions{})
	return err
}

func freePort() (int, error) {
	for i := 0; i < 50; i++ {
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			return 0, err
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if p > 1024 {
			return p, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, errors.New("无法分配可用端口")
}
