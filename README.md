### 实验性Docker Registry 流式代理推送中间件



###  Linux客户端配置

#### 1. Docker Daemon 配置

编辑 `/etc/docker/daemon.json` 文件（如果不存在则创建）：

```bash
sudo vim /etc/docker/daemon.json
```

添加以下配置：

```json
{
  "registry-mirrors": ["http://your-proxy-server:5000"],
  "insecure-registries": ["your-proxy-server:5000"],
  "max-concurrent-downloads": 10,
  "max-concurrent-uploads": 5
}
```

重启Docker服务：
```bash
sudo systemctl restart docker
sudo systemctl status docker  # 验证服务状态
```

#### 2. 用户认证配置（透传模式）

本代理工作在透传认证模式，直接使用您的DockerHub凭据。

##### 方法一：交互式登录
```bash
docker login your-proxy-server:5000
# 输入用户名: your-dockerhub-username
# 输入密码: your-dockerhub-token-or-password
```

##### 方法二：配置文件
```bash
# 创建Docker配置目录
mkdir -p ~/.docker

# 直接创建认证配置
cat > ~/.docker/config.json << 'EOF'
{
  "auths": {
    "your-proxy-server:5000": {
      "username": "your-dockerhub-username",
      "password": "your-dockerhub-token-or-password"
    }
  }
}
EOF

# 设置文件权限
chmod 600 ~/.docker/config.json
```

**注意**: 代理会直接使用您提供的凭据向DockerHub进行认证，无需本地用户管理。


#### 5. 使用示例

```bash
# 基本测试
docker pull alpine:latest
docker tag alpine:latest your-proxy-server:5000/alpine:latest
docker push your-proxy-server:5000/alpine:latest

# 从代理拉取
docker pull your-proxy-server:5000/alpine:latest

# 运行容器
docker run your-proxy-server:5000/alpine:latest echo "Hello from proxy!"
```

## 说明

1. **启动服务器**: `./registry-proxy`
2. **客户端登录**: `docker login localhost:5000`
3. **推送镜像**: `docker push localhost:5000/your-image:tag`
4. **享受高性能代理**: 自动缓存认证，流式传输到DockerHub！

### 配置（可选）
```
{
  // 网络和服务配置
  "port": 5000,                                    // HTTP服务监听端口
  "tls_cert": "",                                  // TLS证书文件路径，空字符串表示使用HTTP模式
  "tls_key": "",                                   // TLS私钥文件路径
  
  // 认证和安全配置
  "token_expiry": "24h",                           // JWT Token过期时间（支持时间格式：24h, 30m, 3600s等）
  
  // 性能优化配置
  "max_concurrent": 50,                            // 最大并发请求数，控制系统负载
  "buffer_size": 1048576,                          // 流式传输缓冲区大小（字节），1MB = 1048576
  "connection_pool_size": 100,                     // HTTP连接池大小，影响上游连接复用
  "request_timeout": "30s",                        // 上游请求超时时间，防止请求挂起
  
  // 监控和日志配置
  "log_level": "info",                            // 日志级别：debug, info, warn, error
  
  // 数据持久化配置
  "data_persistence": false,                       // 是否启用数据持久化到文件
  "data_file": "/tmp/registry-proxy-data.json"     // 持久化数据文件存储路径
}
```