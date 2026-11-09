#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
联通网盘上传速度诊断（在**你的服务器**上直接跑，完全绕开 OpenList / Docker）

用途：判断「上传只有几十 K」到底是
  A. 联通网盘服务端对账号 / 上传节点限速（与 OpenList 无关）
  B. OpenList 容器 / 驱动侧的问题

原理：用你自己的 access token，直接复用 ltpan.py 的 App 通道上传同一文件，
      分别测不同节点、串行/并发的真实吞吐。

用法：
  WOPAN_TOKEN='你的access_token' python3 diagnose.py
  # 或
  python3 diagnose.py --token 你的access_token

依赖：pip3 install requests pycryptodome
"""
import argparse
import importlib.util
import os
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
LTPAN = os.path.join(HERE, "ltpan.py")


def load_ltpan():
    if not os.path.isfile(LTPAN):
        sys.exit(f"缺少 {LTPAN}，请把 ltpan.py 和 diagnose.py 放在同一目录")
    try:
        spec = importlib.util.spec_from_file_location("ltpan", LTPAN)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        return mod
    except ImportError as e:
        sys.exit(f"缺少依赖，请先执行：pip3 install requests pycryptodome\n原始错误：{e}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--token", default=os.environ.get("WOPAN_TOKEN", ""), help="access token")
    ap.add_argument("--size", type=int, default=21, help="测试文件大小 MB，默认 21")
    ap.add_argument("--dir", default="0", help="测试上传目录 id，默认根目录")
    args = ap.parse_args()

    token = args.token.strip()
    if not token:
        token = input("请输入 access token: ").strip()
    if len(token) < 16:
        sys.exit("access token 至少需要 16 位")

    m = load_ltpan()
    cli = m.WoPan(token)

    fd, path = tempfile.mkstemp(prefix="wopan2_diag_", suffix=".bin")
    with os.fdopen(fd, "wb") as f:
        f.write(os.urandom(args.size << 20))

    # (标签, 上传节点, 并发数)
    cases = [
        ("hyupload 串行", "https://hyupload.pan.wo.cn", 1),
        ("hyupload 4并发", "https://hyupload.pan.wo.cn", 4),
        ("tjupload 串行", "https://tjupload.pan.wo.cn", 1),
        ("gxupload 串行", "https://gxupload.pan.wo.cn:8443", 1),
    ]

    print(f"测试文件 {args.size}MB，token 前 8 位 {token[:8]}…，共 {len(cases)} 组\n")
    print(f"{'组合':<20}{'耗时':>10}{'吞吐':>14}  结果")
    print("-" * 62)

    import shutil

    results = []
    for idx, (label, host, workers) in enumerate(cases, 1):
        # 每组用不同文件名，避免同名覆盖
        case_path = f"{path}.{idx}"
        shutil.copyfile(path, case_path)
        t0 = time.time()
        try:
            r = cli.upload(case_path, dir_id=args.dir, chunk_mb=8, workers=workers,
                           use_quick=False, host=host, quiet=True, retries=1)
            elapsed = time.time() - t0
            mbps = args.size / elapsed
            results.append((label, host, workers, mbps))
            print(f"{label:<20}{elapsed:>9.1f}s{mbps:>12.2f} MB/s  {r.get('parts', '?')} 片")
        except Exception as e:
            elapsed = time.time() - t0
            print(f"{label:<20}{elapsed:>9.1f}s{'--':>14}  失败: {type(e).__name__}: {str(e)[:60]}")
        finally:
            try:
                os.remove(case_path)
            except OSError:
                pass

    # 清理网盘里本次产生的测试文件
    try:
        items = cli.ls(args.dir)
        ids = [it.get("id") for it in items
               if str(it.get("name", "")).startswith("wopan2_diag_") and it.get("id")]
        if ids:
            cli.rm(ids)
            print(f"\n已清理网盘测试文件 {len(ids)} 个")
    except Exception as e:
        print(f"\n清理测试文件失败（请手动删除 wopan2_diag_ 开头的文件）：{e}")
    try:
        os.remove(path)
    except OSError:
        pass

    print("\n" + "=" * 62)
    fast = [r for r in results if r[3] >= 3]
    if fast:
        best = max(fast, key=lambda r: r[3])
        print(f"结论：节点 {best[1]}（并发 {best[2]}）可达 {best[3]:.2f} MB/s。")
        print("→ 直连正常，慢是 OpenList 容器/驱动侧问题。把上面的 upload_host /")
        print("  concurrency 填到「联通网盘2」配置，并检查容器日志里的实测吞吐。")
    elif results:
        avg = sum(r[3] for r in results) / len(results)
        print(f"结论：所有组合平均仅 {avg:.2f} MB/s，且不受节点/并发影响。")
        print("→ 这是联通网盘服务端对账号/上传通道的限速，与 OpenList、驱动代码无关。")
        print("  按下面顺序尝试：")
        print("  1) 换一个新的 access token（或 refresh token），旧 token 可能已被风控限速")
        print("  2) 等待一段时间后重试（限流通常按时间窗口恢复）")
        print("  3) 把「联通网盘2」的 concurrency 设为 1、retry_times 设为 0")
        print("  4) 逐个试 upload_host 候选节点")
    else:
        print("结论：所有上传均失败，请检查 token 是否有效、服务器能否访问 pan.wo.cn。")
    print("=" * 62)


if __name__ == "__main__":
    main()
