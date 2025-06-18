# Docker 仓库认证代理 加推送组件

一个高性能的Docker Registry服务，使用Go 1.24和vulcand/oxy v2构建，支持代理Docker Hub认证 和推送镜像。



### 启动服务（服务端）

```bash
# 构建
go build -o dockerpush

# 启动服务
./dockerpush
```

服务将在`5002`端口启动。然你反向代理到域名并开启HTTPS

### 2. Docker客户端使用教程

#### 方法一：直接使用地址（在docker客户端操作即可）
> 将`yourdomain.com`替换为你的反向代理的域名

```bash
# 登录（使用Docker Hub官方仓库的凭据）
docker login yourdomain.com

# 输入用户名，然后输入密码或者token



# 标记镜像
docker tag alpine:latest yourdomain.com/username/alpine:latest

# 推送镜像
docker push yourdomain.com/username/alpine:latest

# 访问Docker hub页面查看推送成功
```




### 生产环境部署

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

```
docker compose up -d
```
