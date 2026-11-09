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

升级到最新版：
  docker load -i openlist-wopan2.tar.gz
  docker compose up -d --force-recreate
  # 或从源码： docker compose up -d --build

添加存储：管理后台 -> 存储 -> 添加 -> 驱动选择「联通网盘2」
  必填：access_token（或 refresh_token）
  upload_host：https://tjupload.pan.wo.cn   ← 关键，见下方
  chunk_size：8      concurrency：4（节点被限速时可设为 1）
  enable_quick：保持关闭

【v3 结论：离线下载上传只有几十 K 的真正原因】
  不是秒传，也不是分片算法，而是「上传节点」被服务端限速。
  同一 token、同一 App headers、同一 upload2C 接口、同一 21MB 文件实测：
    https://hyupload.pan.wo.cn  -> 0.26~0.36 MB/s（ltpan.py 默认节点，已限速）
    https://tjupload.pan.wo.cn  -> 5.0~7.0 MB/s（约 20 倍）
  SDK 的 GetZoneInfo 也会返回 hyupload，所以原「联通网盘」驱动的网页通道
  实际打在同一被限速节点；用户自己的 Python 原程序打 hyupload 也只有
  0.8MB/s。即与 OpenList / 驱动代码无关，改分片、并发、秒传、换 token 都无效。

★ 建议做的一步（新版已能自动迁移，改成明确值最稳妥）：
  管理后台 -> 编辑「联通网盘2」存储，把 upload_host 设为：
      https://tjupload.pan.wo.cn
  新版代码会自动把旧默认值 hyupload 迁移到 tjupload，因此只重建镜像、
  不改配置也能生效；但已保存的配置本身不会变，显式填好可避免歧义。

★ 想确认你的服务器上哪个节点最快（不同网络/运营商结论可能不同）：
  WOPAN_TOKEN='你的access_token' python3 tools/wopan2/diagnose.py
  （脚本与 ltpan.py 已随仓库放在 tools/wopan2/；依赖 pip3 install requests pycryptodome）
  脚本会直连测多个节点并自动清理测试文件，把最快节点填到 upload_host。

验证是否生效：
  docker logs openlist 2>&1 | grep 联通网盘2
  会打印「存储生效配置 | 节点=... 分片=... 并发=...」，以及每次上传的
  「上传完成: ... 实测 X MB/s | 生效配置: 节点=...」。
  若实测仍低于 1.5MB/s，日志会输出 WARN 并提示更换节点。
EOF
