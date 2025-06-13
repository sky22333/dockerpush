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

#### 2. 用户认证配置

##### 方法一：交互式登录
```bash
docker login your-proxy-server:5000
# 输入用户名: admin
# 输入密码: admin123
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
      "username": "admin",
      "password": "admin123"
    }
  }
}
EOF

# 设置文件权限
chmod 600 ~/.docker/config.json
```


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

