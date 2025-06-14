# Docker Registry 流式代理服务器

高性能Docker镜像代理推送中间件，支持流式传输、零内存认证透传、智能上游路由。

## ✨ 核心特性

- 🚀 **高性能流式代理** - 零拷贝流式传输，支持大镜像
- 🔐 **零内存认证透传** - JWT嵌入式认证，无用户数据存储
- ⚡ **智能并发控制** - 分片会话管理，支持高并发上传
- 🎯 **多上游路由** - 智能路由到DockerHub、GCR、Harbor等
- 📊 **生产就绪** - 健康检查、指标监控、优雅停机

## 🚀 快速开始

### 服务端部署

#### 1. 编译运行
```bash
# 克隆项目
git clone <repository-url>
cd dockerpush

# 编译
go build -o registry-proxy .

# 启动服务
./registry-proxy
```

#### 2. 配置文件（可选）
创建 `config.json`：
```json
{
  "port": 5000,                                    // HTTP服务监听端口
  "tls_cert": "",                                  // TLS证书文件路径（启用HTTPS）
  "tls_key": "",                                   // TLS私钥文件路径（启用HTTPS）
  "token_expiry": "24h",                           // JWT Token过期时间（支持：24h, 1440m, 86400s）
  "max_concurrent": 50,                            // 最大并发上传数（信号量控制）
  "buffer_size": 1048576,                          // 流式传输缓冲区大小（字节，1MB）
  "log_level": "info",                             // 日志级别（debug, info, warn, error）
  "data_persistence": false,                       // 是否启用数据持久化
  "data_file": "/tmp/registry-proxy-data.json",    // 数据持久化文件路径
  "connection_pool_size": 100,                     // HTTP连接池大小（分片数量）
  "request_timeout": "60s",                        // 上游请求超时时间
  "upstreams": [                                   // 上游注册表配置列表（按优先级排序）
    {
      "name": "gcr",                               // Google容器镜像服务
      "registry": "gcr.io", 
      "route_pattern": "gcr.io/*"                 // 优先级1：匹配gcr.io/开头的仓库
    },
    {
      "name": "harbor",                            // 私有Harbor仓库
      "registry": "harbor.company.com",
      "route_pattern": "private/*"                // 优先级2：匹配private/开头的仓库
    },
    {
      "name": "dockerhub",                         // DockerHub（不指定路径则使用dockerhub）
      "registry": "index.docker.io",              // 注册表地址
      "route_pattern": "*"                        // 优先级3：（默认仓库）
    }
  ]
}
```

### 客户端配置

#### 1. Docker配置
编辑 `/etc/docker/daemon.json`：
```json
{
  "registry-mirrors": ["http://your-server:5000"],
  "insecure-registries": ["your-server:5000"]
}
```

重启Docker：
```bash
sudo systemctl restart docker
```

#### 2. 用户认证
使用您的DockerHub凭据登录：
```bash
docker login your-server:5000
# 输入DockerHub用户名和密码/Token
```

## 📖 使用示例

### 基础操作
```bash
# 推送镜像
docker tag alpine:latest your-server:5000/alpine:latest
docker push your-server:5000/alpine:latest

# 拉取镜像
docker pull your-server:5000/alpine:latest

# 查看镜像列表
curl http://your-server:5000/v2/_catalog
```

### 高级用法
```bash
# 推送到不同上游（基于仓库名称路由）
docker push your-server:5000/gcr.io/project/image:tag    # 路由到GCR
docker push your-server:5000/private/image:tag          # 路由到Harbor
docker push your-server:5000/image:tag                  # 路由到DockerHub
```

## 🔧 配置说明

### 核心配置
| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `port` | 5000 | 服务监听端口 |
| `max_concurrent` | 50 | 最大并发数 |
| `buffer_size` | 1MB | 流式缓冲区大小 |
| `token_expiry` | 24h | JWT过期时间 |
| `request_timeout` | 30s | 上游请求超时 |

### 上游配置

**配置文件方式（推荐）：**
在 `config.json` 中配置 `upstreams` 数组：

| 字段 | 类型 | 说明 | 示例 |
|------|------|------|------|
| `name` | string | 上游名称（唯一标识） | "dockerhub" |
| `registry` | string | 注册表地址 | "index.docker.io" |
| `route_pattern` | string | 路由匹配模式 | "gcr.io/*" |

**路由匹配规则：**
- `*` - 匹配所有仓库（默认上游）
- `gcr.io/*` - 匹配gcr.io开头的仓库
- `private/*` - 匹配private开头的仓库

**路由优先级：**
- 按配置文件中的数组顺序进行匹配
- 第一个匹配的模式将被使用
- 建议将具体模式放在前面，通配符`*`放在最后作为默认仓库


## 🏥 健康检查

```bash
# 服务状态
curl http://your-server:5000/health

# API版本
curl http://your-server:5000/v2/

# 指标监控
curl http://your-server:5000/metrics
```

## 🔒 安全说明

- **认证透传**: 代理不存储用户凭据，直接透传到上游
- **JWT安全**: 使用HMAC签名保护Token完整性
- **TLS支持**: 配置证书启用HTTPS加密
- **访问控制**: 基于Docker Registry标准的权限控制

## 🐛 故障排除

### 常见问题

**1. 连接被拒绝**
```bash
# 检查服务状态
curl http://localhost:5000/health


**2. 认证失败**
```bash
# 重新登录
docker logout your-server:5000
docker login your-server:5000
```

**3. 推送超时**
```bash
# 增加超时时间
export DOCKER_CLIENT_TIMEOUT=120
export COMPOSE_HTTP_TIMEOUT=120
```

### 日志调试
```bash
# 启用调试日志
export LOG_LEVEL=debug
./registry-proxy

# 查看详细日志
./registry-proxy 2>&1 | tee registry.log
```

## 📊 性能优化

### 服务端优化
- 调整 `max_concurrent` 控制并发数
- 增大 `buffer_size` 提升大文件传输性能
- 配置 `connection_pool_size` 优化连接复用

### 客户端优化
```json
{
  "max-concurrent-downloads": 10,
  "max-concurrent-uploads": 5,
  "max-download-attempts": 3
}
```

## 🔄 版本信息

```bash
./registry-proxy -version
```
