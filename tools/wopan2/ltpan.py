#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
联通网盘（沃盘 / pan.wo.cn）App 通道上传工具

原理：
  App 走的是「上传专用节点」hyupload.pan.wo.cn 的 /openapi/client/upload2C，
  该节点不做网页端那种 200KB/s 的限速；本脚本复用同一条通道。

用法：
  export WOPAN_TOKEN=xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
  python3 ltpan.py info
  python3 ltpan.py upload ./movie.mkv --dir 3083ba2e47774559a8dfef6a14dcd6ea
  python3 ltpan.py ls 0
  python3 ltpan.py mkdir 0 我的电影
  python3 ltpan.py rm <fileId>

依赖：pip install requests pycryptodome
"""

import argparse
import base64
import hashlib
import json
import os
import random
import string
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed

import requests
from Crypto.Cipher import AES
from requests.adapters import HTTPAdapter

# ---------------------------------------------------------------- 常量
BASE_URL = "https://s.pan.wo.cn"          # 业务网关（dispatcher）
UPLOAD_HOST = "https://hyupload.pan.wo.cn"  # 上传专用节点（不限速）
TRANSFER_HOST = "https://b.smartont.net"    # 秒传 / 文件校验
AES_IV = b"wNSOYIB1k1DjY5lA"
CLIENT_SECRET = "XFmi9GS2hzk98jGX"          # 网页/开放端固定密钥
DEFAULT_CLIENT_ID = "1001000035"            # App 的 Client-Id
APP_UA = "LianTongYunPan/6.5.0 (Android 16)"
OKGO_UA = "okhttp-okgo/jeasonlzy LianTongYunPan/6.5.0 (Android 16)"


# ---------------------------------------------------------------- 工具
def human(n: float) -> str:
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024 or unit == "TB":
            return f"{n:.1f}{unit}" if unit != "B" else f"{int(n)}B"
        n /= 1024


def ext_type(name: str) -> str:
    """服务端文件类型：1图片 2音频 3视频 4文档 5其它（传 5 服务端也会自行纠正）"""
    ext = os.path.splitext(name)[1].lower().lstrip(".")
    if ext in {"jpg", "jpeg", "png", "gif", "bmp", "webp", "heic"}:
        return "1"
    if ext in {"mp3", "wav", "flac", "aac", "m4a", "ogg"}:
        return "2"
    if ext in {"mp4", "mkv", "avi", "mov", "flv", "ts", "rmvb", "wmv"}:
        return "3"
    if ext in {"doc", "docx", "xls", "xlsx", "ppt", "pptx", "pdf", "txt", "md", "csv"}:
        return "4"
    return "5"


def sha256_file(path: str, bufsize: int = 4 << 20) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            b = f.read(bufsize)
            if not b:
                break
            h.update(b)
    return h.hexdigest()


class Progress:
    def __init__(self, total: int):
        self.total = total
        self.done = 0
        self.lock = threading.Lock()
        self.t0 = time.time()
        self.last = 0.0

    def add(self, n: int):
        with self.lock:
            self.done += n
            now = time.time()
            if now - self.last < 0.2 and self.done < self.total:
                return
            self.last = now
            pct = self.done * 100 / self.total if self.total else 100
            speed = self.done / max(now - self.t0, 1e-6)
            bar = "█" * int(pct / 4) + "░" * (25 - int(pct / 4))
            sys.stdout.write(
                f"\r  [{bar}] {pct:5.1f}%  {human(self.done)}/{human(self.total)}  {human(speed)}/s"
            )
            sys.stdout.flush()

    def finish(self, note: str = ""):
        el = time.time() - self.t0
        sys.stdout.write(
            f"\r  [{'█' * 25}] 100.0%  {human(self.total)}  用时 {el:.1f}s"
            f"  平均 {human(self.total / el if el else 0)}/s {note}\n"
        )
        sys.stdout.flush()


# ---------------------------------------------------------------- 客户端
class WoPan:
    def __init__(self, token: str, device_id: str = "", client_id: str = DEFAULT_CLIENT_ID,
                 proxies: dict | None = None, timeout: int = 60):
        if not token or len(token) < 16:
            raise ValueError("access token 至少 16 位")
        self.token = token.strip()
        self.device_id = device_id or "".join(
            random.choices(string.ascii_letters + string.digits + "/+", k=32)
        )
        self.client_id = client_id
        self.key = self.token[:16].encode()   # 关键：AES key = token 前 16 位
        self.timeout = timeout
        self.proxies = proxies or {}

        self.s = requests.Session()
        adapter = HTTPAdapter(pool_connections=16, pool_maxsize=32, max_retries=0)
        self.s.mount("https://", adapter)
        self.s.mount("http://", adapter)

    # ---------- 加解密 ----------
    def encrypt(self, obj, key: bytes | None = None) -> str:
        raw = json.dumps(obj, separators=(",", ":"), ensure_ascii=False).encode()
        pad = 16 - len(raw) % 16
        raw += bytes([pad]) * pad
        return base64.b64encode(
            AES.new(key or self.key, AES.MODE_CBC, AES_IV).encrypt(raw)
        ).decode()

    def decrypt(self, b64: str, key: bytes | None = None):
        raw = base64.b64decode(b64)
        plain = AES.new(key or self.key, AES.MODE_CBC, AES_IV).decrypt(raw)
        plain = plain[: -plain[-1]]
        txt = plain.decode("utf-8", "replace")
        try:
            return json.loads(txt)
        except json.JSONDecodeError:
            return txt

    # ---------- 请求头 ----------
    def headers(self, kind: str = "dispatcher") -> dict:
        h = {
            "X-YP-Device-Id": self.device_id,
            "Access-Token": self.token,
            "accesstoken": self.token,
            "Client-Id": self.client_id,
            "App-Version": "yp-app/6.5.0",
            "appversion": "6.5.0",
            "Sys-Version": "Android/16",
            "sys-version": "Android/16",
            "platform": "1",
            "platfomr": "1",
            "app-type": "liantongyunpanapp",
            "bundleid": "com.chinaunicom.bol.cloudapp",
            "appchannel": "other",
            "network-type": "wifi",
            "oaid": "00000000",
        }
        if kind == "upload":
            # 注意：multipart 请求不能手动设 Content-Type，否则 boundary 丢失
            h["User-Agent"] = OKGO_UA
            h["Accept-Language"] = "zh-CN,zh;q=0.8"
            h["Origin"] = "https://pan.wo.cn"
            h["Referer"] = "https://pan.wo.cn/"
        else:
            h["User-Agent"] = APP_UA
        return h

    # ---------- dispatcher ----------
    def _sign(self, key: str, res_time: str, req_seq: str, channel: str, version: str = "") -> str:
        return hashlib.md5(
            f"{key}{res_time}{req_seq}{channel}{version}".encode()
        ).hexdigest().upper()

    def dispatcher(self, key: str, param: dict, extra: dict | None = None,
                   channel: str = "wohome", retries: int = 2) -> dict:
        for attempt in range(retries + 1):
            res_time = str(int(time.time() * 1000))
            req_seq = str(random.randint(100000, 199999))
            header = {
                "key": key,
                "channel": channel,
                "reqSeq": req_seq,
                "resTime": res_time,
                "version": "",
                "sign": self._sign(key, res_time, req_seq, channel),
            }
            body = {"param": self.encrypt(param)}
            if extra:
                body.update(extra)
            try:
                r = self.s.post(
                    f"{BASE_URL}/{channel}/dispatcher",
                    headers=self.headers(),
                    json={"header": header, "body": body},
                    timeout=self.timeout,
                    proxies=self.proxies,
                )
                j = r.json()
                rsp = j.get("RSP", {})
                if rsp.get("RSP_CODE") == "0000":
                    return rsp
                err = f'{rsp.get("RSP_CODE")} {rsp.get("RSP_DESC")}'
            except Exception as e:  # 网络抖动
                err = repr(e)
            if attempt == retries:
                raise RuntimeError(f"{key} 失败: {err}")
            time.sleep(1 + attempt)
        raise RuntimeError("unreachable")

    def call(self, key: str, param: dict, extra: dict | None = None):
        rsp = self.dispatcher(key, param, extra or {"secret": True})
        data = rsp.get("DATA")
        if isinstance(data, str) and data.startswith("{"):   # 未加密的 JSON 文本
            try:
                return json.loads(data)
            except json.JSONDecodeError:
                pass
        if isinstance(data, str) and data:
            try:
                return self.decrypt(data)
            except Exception:
                return data
        return data

    # ---------- 账号 ----------
    def info(self) -> dict:
        out = {}
        try:
            out["usage"] = self.call("QueryCloudUsageInfo", {
                "spaceType": "0", "clientId": self.client_id,
                "familyId": 0})
        except Exception as e:
            out["usage_error"] = str(e)
        return out

    # ---------- token 刷新（需要 refresh_token，未做实测） ----------
    def refresh(self, refresh_token: str) -> dict:
        """用 refresh_token 换新的 access_token；返回 {access_token, refresh_token}"""
        param = {"refreshToken": refresh_token, "clientSecret": CLIENT_SECRET}
        res_time = str(int(time.time() * 1000))
        req_seq = str(random.randint(100000, 199999))
        header = {
            "key": "AppRefreshToken",
            "channel": "api-user",
            "reqSeq": req_seq,
            "resTime": res_time,
            "version": "",
            "sign": self._sign("AppRefreshToken", res_time, req_seq, "api-user"),
        }
        body = {
            "param": self.encrypt(param, CLIENT_SECRET.encode()),
            "clientId": "1001000021",
            "secret": True,
        }
        r = self.s.post(f"{BASE_URL}/api-user/dispatcher",
                        headers=self.headers(), json={"header": header, "body": body},
                        timeout=self.timeout, proxies=self.proxies)
        rsp = r.json().get("RSP", {})
        if rsp.get("RSP_CODE") != "0000":
            raise RuntimeError(f'{rsp.get("RSP_CODE")} {rsp.get("RSP_DESC")}')
        data = rsp.get("DATA")
        if isinstance(data, str) and data.startswith("{"):
            return json.loads(data)
        if isinstance(data, str) and data:
            return self.decrypt(data, CLIENT_SECRET.encode())
        return data or {}

    # ---------- 目录 ----------
    def ls(self, dir_id: str = "0", space_type: str = "0", family_id: int = 0) -> list:
        files, page = [], 0
        while True:
            data = self.call("QueryAllFiles", {
                "spaceType": space_type,
                "parentDirectoryId": dir_id,
                "pageNum": page,
                "pageSize": 100,
                "sortRule": 1,
                "clientId": self.client_id,
                "familyId": family_id,
            })
            batch = (data or {}).get("files", []) or []
            files.extend(batch)
            if len(batch) < 100:
                break
            page += 1
        return files

    def mkdir(self, parent_id: str, name: str, space_type: str = "0") -> str:
        data = self.call("CreateDirectory", {
            "spaceType": space_type,
            "parentDirectoryId": parent_id,
            "directoryName": name,
            "familyId": 0,
            "clientId": self.client_id,
        })
        return (data or {}).get("id", "")

    def rm(self, file_ids: list[str], space_type: str = "0"):
        return self.call("DeleteFile", {
            "spaceType": space_type,
            "vipLevel": "0",
            "dirList": [],
            "fileList": file_ids,
            "clientId": self.client_id,
        })

    # ---------- 秒传 ----------
    def quick_transfer(self, name: str, size: int, sha: str, dir_id: str,
                       space_type: str = "0", mtime: float | None = None) -> dict:
        import uuid
        body = {
            "batchNo": uuid.uuid4().hex.upper()[:32],
            "deviceId": "",
            "directoryId": dir_id,
            "familyId": 0,
            "fileModificationTime": int((mtime or time.time()) * 1000),
            "fileName": name,
            "fileSize": str(size),
            "fileType": ext_type(name),
            "height": "", "lat": "", "lng": "",
            "psToken": "",
            "sha256": sha,
            "spaceType": space_type,
            "width": "",
        }
        h = self.headers("upload")
        h["client-Id"] = self.client_id
        h["app-version"] = "yp-app/6.5.0"
        h["access-token"] = self.token
        r = self.s.post(f"{TRANSFER_HOST}/openapi/transfer/quickTransfer",
                        headers=h, json=body, timeout=self.timeout, proxies=self.proxies)
        return r.json().get("result", {}) or {}

    # ---------- 上传 ----------
    def upload(self, path: str, dir_id: str = "0", chunk_mb: int = 16, workers: int = 4,
               space_type: str = "0", family_id: int = 0, use_quick: bool = True,
               host: str = UPLOAD_HOST, quiet: bool = False, retries: int = 3) -> dict:
        path = os.path.abspath(path)
        if not os.path.isfile(path):
            raise FileNotFoundError(path)
        name = os.path.basename(path)
        size = os.path.getsize(path)
        mtime = os.path.getmtime(path)
        batch_no = time.strftime("%Y%m%d%H%M%S")

        if not quiet:
            print(f"文件: {name}\n大小: {human(size)}  分片: {chunk_mb}MB × {workers} 并发")

        # 1) 秒传
        sha = ""
        if use_quick and size > 0:
            if not quiet:
                sys.stdout.write("计算 sha256 ...")
                sys.stdout.flush()
            t = time.time()
            sha = sha256_file(path)
            if not quiet:
                print(f" {time.time() - t:.1f}s")
            try:
                res = self.quick_transfer(name, size, sha, dir_id, space_type, mtime)
                if str(res.get("hasFile")) == "1" and res.get("fileId"):
                    if not quiet:
                        print(f"⚡ 秒传命中，fileId={res['fileId']}")
                    return {"fid": res["fileId"], "quick": True, "size": size, "name": name}
            except Exception as e:
                if not quiet:
                    print(f"  秒传检查跳过: {e}")

        # 2) 注册分片任务
        part_size = max(chunk_mb, 1) * 1024 * 1024
        total_part = max(1, -(-size // part_size))
        file_info = {
            "spaceType": space_type,
            "directoryId": dir_id,
            "batchNo": batch_no,
            "fileName": name,
            "fileSize": size,
            "fileType": ext_type(name),
        }
        if space_type == "1":
            file_info["familyId"] = family_id
        unique_id = f"{int(time.time() * 1000)}_{''.join(random.choices(string.ascii_letters, k=6))}"

        form = {
            "uniqueId": unique_id,
            "accessToken": self.token,
            "fileName": name,
            "psToken": "undefined",
            "fileSize": str(size),
            "totalPart": str(total_part),
            "channel": "wocloud",
            "directoryId": dir_id,
            "fileInfo": self.encrypt(file_info),
        }
        url = f"{host}/openapi/client/upload2C"
        progress = Progress(size)
        fid_box: list[str] = []
        box_lock = threading.Lock()

        def send(idx: int):
            offset = (idx - 1) * part_size
            length = min(part_size, size - offset)
            last_err = None
            for attempt in range(retries):
                try:
                    with open(path, "rb") as f:
                        f.seek(offset)
                        payload = f.read(length)
                    data = dict(form)
                    data["partIndex"] = str(idx)
                    data["partSize"] = str(length)
                    r = self.s.post(
                        url, headers=self.headers("upload"), data=data,
                        files={"file": (name, payload, "application/octet-stream")},
                        timeout=(20, 900), proxies=self.proxies,
                    )
                    j = r.json()
                    if j.get("code") != "0000":
                        raise RuntimeError(f'{j.get("code")} {j.get("msg")}')
                    f_id = ((j.get("data") or {}).get("fid") or "")
                    if f_id:
                        with box_lock:
                            if f_id not in fid_box:
                                fid_box.append(f_id)
                    progress.add(length)
                    return idx
                except Exception as e:
                    last_err = e
                    time.sleep(2 * (attempt + 1))
            raise RuntimeError(f"分片 {idx}/{total_part} 失败: {last_err}")

        # 3) 并发上传
        t0 = time.time()
        with ThreadPoolExecutor(max_workers=max(1, min(workers, total_part))) as ex:
            futs = [ex.submit(send, i) for i in range(1, total_part + 1)]
            for fu in as_completed(futs):
                fu.result()
        if not quiet:
            progress.finish("(含服务端合并)" if total_part > 1 else "")
            print(f"上传完成，用时 {time.time() - t0:.1f}s")

        return {"fid": fid_box[0] if fid_box else "", "quick": False,
                "size": size, "name": name, "parts": total_part}


# ---------------------------------------------------------------- CLI
def get_token(args) -> str:
    tok = args.token or os.environ.get("WOPAN_TOKEN", "")
    if not tok:
        cfg = os.path.expanduser("~/.wopan.json")
        if os.path.exists(cfg):
            try:
                tok = json.load(open(cfg)).get("access_token", "")
            except Exception:
                pass
    if not tok:
        sys.exit("缺少 access token：用 --token 或环境变量 WOPAN_TOKEN 指定")
    return tok


def main():
    ap = argparse.ArgumentParser(description="联通网盘 App 通道上传工具")
    ap.add_argument("--token", help="access token（默认读 WOPAN_TOKEN 环境变量）")
    ap.add_argument("--device-id", default="", help="X-YP-Device-Id，可留空")
    ap.add_argument("--proxy", default="", help="如 http://127.0.0.1:7890")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("upload", help="上传文件")
    p.add_argument("file")
    p.add_argument("--dir", default="0", help="目标目录 id，默认根目录")
    p.add_argument("--chunk", type=int, default=16, help="分片大小 MB，默认 16")
    p.add_argument("--workers", type=int, default=4, help="并发数，默认 4")
    p.add_argument("--space", default="0", help="0 个人 1 家庭")
    p.add_argument("--family-id", type=int, default=0)
    p.add_argument("--no-quick", action="store_true", help="跳过秒传检查")
    p.add_argument("--host", default=UPLOAD_HOST, help="上传节点")

    p = sub.add_parser("ls", help="列出目录")
    p.add_argument("dir", nargs="?", default="0")

    p = sub.add_parser("mkdir", help="新建文件夹")
    p.add_argument("parent", nargs="?", default="0")
    p.add_argument("name")

    p = sub.add_parser("rm", help="删除文件（按 fileId）")
    p.add_argument("ids", nargs="+")

    sub.add_parser("info", help="账号信息")

    args, unknown = ap.parse_known_args()

    # 允许 --token / --proxy 写在子命令后面（argparse 全局选项默认必须前置）
    i = 0
    while i < len(unknown):
        if unknown[i] in ("--token", "--proxy", "--device-id") and i + 1 < len(unknown):
            setattr(args, unknown[i][2:].replace("-", "_"), unknown[i + 1])
            i += 2
        else:
            i += 1

    proxies = {"http": args.proxy, "https": args.proxy} if args.proxy else None
    cli = WoPan(get_token(args), args.device_id, proxies=proxies)

    if args.cmd == "upload":
        res = cli.upload(args.file, args.dir, args.chunk, args.workers,
                         args.space, args.family_id, not args.no_quick, args.host)
        print(json.dumps(res, ensure_ascii=False))
    elif args.cmd == "ls":
        files = cli.ls(args.dir)
        print(f"{'类型':<4}{'大小':>10}  {'fileId':<34}名称")
        for f in files:
            kind = "目录" if f.get("type") == 0 else "文件"
            print(f"{kind:<4}{human(f.get('size', 0)):>10}  {f.get('id', ''):<34}{f.get('name')}")
        print(f"共 {len(files)} 项")
    elif args.cmd == "mkdir":
        print("目录 id:", cli.mkdir(args.parent, args.name))
    elif args.cmd == "rm":
        cli.rm(args.ids)
        print("已删除", len(args.ids), "项")
    elif args.cmd == "info":
        print(json.dumps(cli.info(), ensure_ascii=False, indent=2)[:2000])


if __name__ == "__main__":
    main()
