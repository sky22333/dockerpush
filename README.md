# 实验性Docker镜像代理推送中间件



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

