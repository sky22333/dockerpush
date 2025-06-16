# Docker 仓库认证代理 加推送组件

一个高性能的Docker Registry代理服务，使用Go 1.24和vulcand/oxy v2构建，支持代理Docker Hub认证 和推送镜像。

## 组件实现

### 主要功能模块

#### 1. 认证代理 (`handleAuth`)
- 拦截Docker Hub的401认证响应
- 重写`Www-Authenticate`头部，将realm指向代理端点
- 中继认证请求到原始Docker Hub认证服务
- 透传JWT token给客户端

#### 2. CDN检测与优化 (`isExternalCDN`)
- 智能检测Docker Hub CDN域名
- 支持cloudflare.docker.com等CDN域名
- 自动路由CDN请求，提升下载速度

#### 3. Location重写 (`rewriteLocationHeader`)
- 自动重写重定向URL
- 处理CDN重定向和普通重定向
- 保持客户端URL一致性

#### 4. 流式代理 (`proxyHandler`)
- 基于vulcand/oxy实现高性能转发
- 支持大文件流式传输
- 低内存占用，适合大镜像推送

### 技术特性
- **零拷贝流式传输**: 直接转发数据流，减少内存占用
- **智能CDN路由**: 自动识别并优化CDN请求
- **透明认证**: 对Docker客户端完全透明
- **错误恢复**: 完善的错误处理和日志记录

## Linux客户端使用

### 1. 启动代理服务

```bash
# 构建
go build -o dockerpush

# 启动服务
./dockerpush
```

服务将在`5002`端口启动。然你反向代理到域名并开启HTTPS

### 2. Docker客户端使用教程

#### 方法一：直接使用代理地址（在docker客户端操作即可）
> 将`yourdomain.com`替换为你的反向代理的域名

```bash
# 登录（使用Docker Hub凭据）
docker login yourdomain.com

# 输入用户名，然后输入密码或者token



# 标记镜像
docker tag alpine:latest yourdomain.com/username/alpine:latest

# 推送镜像
docker push yourdomain.com/username/alpine:latest

# 访问Docker hub页面查看推送成功
```



### 2. 健康检查（可忽略）

用户查看当前组件是否正常运行

```bash
# 直接访问
yourdomain.com

# 预期输出
{"status":"ok","service":"docker-registry-proxy","upstream":"registry-1.docker.io"}
```

### 3. 生产环境部署

#### 使用systemd服务

```bash
# 创建服务文件
sudo tee /etc/systemd/system/dockerpush.service > /dev/null <<EOF
[Unit]
Description=Docker Registry Proxy
After=network.target

[Service]
Type=simple
User=dockerpush
WorkingDirectory=/opt/dockerpush
ExecStart=/opt/dockerpush/dockerpush
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

# 启动服务
sudo systemctl daemon-reload
sudo systemctl enable dockerpush
sudo systemctl start dockerpush
```

#### 使用Docker运行

```bash
# 构建镜像
docker build -t dockerpush .

# 运行容器
docker run -d \
  --name dockerpush \
  --restart unless-stopped \
  -p 5002:5002 \
  dockerpush
```

## 配置参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `UPSTREAM_REGISTRY_HOST` | `registry-1.docker.io` | 上游Docker Hub地址 |
| `PROXY_LISTEN_ADDR` | `:5002` | 代理监听端口 |

## 性能特性

- **内存占用**: 流式传输，内存使用恒定
- **并发支持**: 基于goroutine，支持高并发
- **传输效率**: 零拷贝数据转发