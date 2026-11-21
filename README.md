# Harborlight

基于 **Go** 的网页版多 Docker 实例托管面板——直接面向最终用户：登录后即可一键部署 Redis / PostgreSQL / SQLite (sqlite-web) / 自定义镜像，并管理容器的完整生命周期。

![stack](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go) ![docker](https://img.shields.io/badge/Docker-Engine%20API-blue?logo=docker) ![tailwind](https://img.shields.io/badge/UI-TailwindCSS-38BDF8?logo=tailwindcss)

## 功能

- **服务托管**：一键创建 Redis、PostgreSQL、SQLite（sqlite-web 网页管理器）或任意自定义镜像实例，自动拉取镜像、分配主机端口、生成凭据、挂载持久化数据卷
- **完整生命周期**：启动 / 停止 / 重启 / 删除，实时状态同步（Docker Engine 轮询）
- **日志查看**：页面自动轮询容器日志（支持 tail 任意行数）
- **OAuth 登录**：GitHub OAuth 开箱即用，或任意标准 OAuth2/OIDC Provider（环境变量配置）
- **多存储后端**：面板元数据库支持 **SQLite（默认，零配置）** 与 **PostgreSQL**；配置 `HL_REDIS_URL` 时会话写入 **Redis**（天然支持面板多实例横向扩展）
- **多用户隔离**：实例归属创建者，普通用户只能操作自己的实例（admin 可管理全部）
- **UI**：Tailwind CSS 深色主题，响应式

## 快速开始（本地体验）

```bash
# 1. 确认 Docker Desktop 正在运行
docker version

# 2. 启动（演示登录模式，首次运行自动建库）
HL_DEMO_LOGIN=true go run .          # 或 ./harborlight.exe

# 3. 浏览器打开
# http://localhost:8080  -> 点击「本地演示登录」
```

## 多设备 Docker 管理

管理员可以在仪表盘的“Docker 设备”区域登记多台远程 Engine。每台设备需要配置 Docker Engine 的双向 TLS TCP API（通常为 `tcp://设备地址:2376`），并在 Harborlight 主机上提供 CA、客户端证书和客户端私钥文件路径。添加时面板会先调用 Engine Ping 验证连接。

远程设备需要开启 Docker Engine TLS 验证，并在网络防火墙中只允许 Harborlight 主机访问 2376 端口。不要将未启用 TLS 的 Docker API 暴露到网络。示例 daemon 参数：

```text
dockerd --tlsverify \
  --tlscacert=/etc/docker/ca.pem \
  --tlscert=/etc/docker/server-cert.pem \
  --tlskey=/etc/docker/server-key.pem \
  -H tcp://0.0.0.0:2376
```

登记设备时填写远端 Docker 主机上的 Linux 绝对数据目录，例如 `/var/lib/harborlight`。新建实例时在“运行设备”中选择目标设备；实例列表、状态、日志及启停/删除操作都会通过所属 Engine 执行。远程主机端口由该 Docker Engine 自动分配。

设备证书路径保存在 Harborlight 元数据库中，因此应限制数据库和证书文件的访问权限。移除设备前，必须先删除该设备下的面板实例记录。OAuth 部署中可设置 `HL_ADMIN_EMAILS=admin@example.com,ops@example.com` 指定管理员；本地演示登录用户默认是管理员。

## 配置（环境变量）

| 变量 | 说明 | 默认 |
|---|---|---|
| `HL_ADDR` | HTTP 监听地址 | `:8080` |
| `HL_DB` | 元数据库：留空=SQLite；`postgres://user:pass@host:5432/db` = PostgreSQL | SQLite |
| `HL_REDIS_URL` | 会话存储：`redis://host:6379/0`，留空=进程内存 | 空 |
| `HL_SESSION_SECRET` | 会话签名密钥（生产必改） | 开发默认值 |
| `HL_BASE_URL` | OAuth 回调基准，如 `https://panel.example.com` | `http://localhost:8080` |
| `HL_DATA_DIR` | 实例数据卷根目录 | `./data/volumes` |
| `HL_OAUTH_PROVIDER` | `github` 或 `generic` | 空 |
| `HL_CLIENT_ID` / `HL_CLIENT_SECRET` | OAuth 应用凭据 | 空 |
| `HL_AUTH_URL` / `HL_TOKEN_URL` / `HL_USERINFO_URL` | generic OAuth2 三件套 | 空 |
| `HL_SCOPES` | OAuth scope | github: `read:user user:email` |
| `HL_DEMO_LOGIN` | `true` 开启本地演示登录（仅测试） | `false` |
| `HL_ADMIN_EMAILS` | 逗号分隔的 OAuth 管理员邮箱白名单 | 空 |

### GitHub OAuth 示例

1. GitHub → Settings → Developer settings → OAuth Apps → New
2. Callback URL 填 `http://localhost:8080/auth/callback`
3. 启动：

```bash
HL_OAUTH_PROVIDER=github \
HL_CLIENT_ID=Iv1.xxxx HL_CLIENT_SECRET=ghp_xxx \
HL_BASE_URL=http://localhost:8080 \
./harborlight.exe
```

### 全套云存储后端示例（SQLite 元数据 + Redis 会话 + PostgreSQL 托管目标）

```bash
HL_DB=postgres://postgres:pass@localhost:5432/harborlight
HL_REDIS_URL=redis://localhost:6379/0
./harborlight.exe
```

## 架构

```
main.go                      装配：config → store → redis → docker → auth → server
internal/
  config/    环境变量配置
  store/     GORM 模型 (User, Instance) + SQLite/PostgreSQL 驱动 + DAO
  auth/      OAuth (github/generic) + 会话（Redis 或内存）+ CSRF state
  dockerx/   Docker SDK 封装：预置规格、端口分配、生命周期、日志
  server/    chi 路由、处理器、嵌入模板 (html/template + Tailwind CDN)
```

每个托管实例：
- 容器命名 `hl-<name>-<id8>`，带 `harborlight.managed` 标签
- 自动分配空闲主机端口，`unless-stopped` 重启策略
- 独立数据卷 `<data_dir>/<instance_id>` 挂载到容器 `/data`
- Redis/Postgres/SQLite 实例创建时自动生成强密码（页面仅展示一次）

## 生产部署建议

- 设置强随机 `HL_SESSION_SECRET`
- OAuth 回调走 HTTPS（`HL_BASE_URL=https://...`）
- 元数据库切换 PostgreSQL、会话接入 Redis，即可多副本部署面板（无状态 + 共享会话）
- 容器级资源限额（CPU/内存）可按需在 `dockerx.Create` 中扩展 `Resources` 字段
