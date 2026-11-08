#!/bin/sh
# 在服务器上启动带「联通网盘2」驱动的 OpenList。
# 用法： sh deploy.sh
set -e
cd "$(dirname "$0")"
mkdir -p data

echo "=== OpenList（联通网盘2 高速上传驱动）==="
if docker image inspect openlist-wopan2:local >/dev/null 2>&1; then
  echo "检测到预构建镜像 openlist-wopan2:local，直接启动（不联网、不编译）..."
  docker compose up -d
else
  echo "未检测到本地镜像，改从源码构建（需要能访问 Go 模块代理与 GitHub Release）..."
  docker compose up -d --build
fi

cat <<'EOF'

启动完成！
  访问地址： http://<服务器IP>:5244
  初始管理员密码： docker logs openlist 2>&1 | grep -i password
  重设管理员密码： docker exec -it openlist ./openlist admin set 你的新密码

添加存储：管理后台 -> 存储 -> 添加 -> 驱动选择「联通网盘2」
  必填：access_token（或 refresh_token）
  可选：chunk_size（默认16MB，最小8MB）、concurrency（默认4）、enable_quick（秒传）
  上传节点 upload_host 默认已是 App 高速节点 https://hyupload.pan.wo.cn，请勿改成网页节点。
EOF
