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

升级到修复版：
  docker load -i openlist-wopan2.tar.gz
  docker compose up -d --force-recreate

添加存储：管理后台 -> 存储 -> 添加 -> 驱动选择「联通网盘2」
  必填：access_token（或 refresh_token）
  可选：chunk_size（默认 8MB）、concurrency（默认 4）
  秒传 enable_quick 默认关闭，离线下载 / URL 转存请保持关闭。
  上传节点 upload_host 默认已是 App 高速节点 https://hyupload.pan.wo.cn，请勿改成网页节点。

【v2 修复：离线下载上传只有几十 K】
  根因不是秒传（关掉秒传无效），而是分片算法错位：
  旧版照搬 SDK 的 floor 算法，文件大小不是分片大小的整数倍时，最后一片会
  膨胀成「分片大小 + 余数」（默认 16MB 下可达 20~32MB），被 hyupload 节点
  稳定限在约 350KB/s；此前所有高速实测恰好都是 8MB 整数倍，所以没暴露。
  现在改为与 ltpan.py 一致的 ceil 算法，每片严格 <= chunk_size：
    20MB: 56.2s(0.36MB/s) -> 2.5s(7.9MB/s)
    40MB: 112.7s(0.35MB/s) -> 3.7s(10.8MB/s)
  升级镜像后即生效，不需要改动任何已保存配置。
  仍建议把旧配置里的 chunk_size 从 16 手动改成 8（16 现在也能正常工作）。

  诊断：compose 里加环境变量 WOPAN2_DEBUG_PARTS=1，日志会打印每个分片耗时、
        分片布局与本次上传的实测吞吐，便于确认瓶颈。
EOF
