package wopan2

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 前 16 位 = "0123456789abcdef"，与 ltpan.py 加密所用的 key 一致。
const testToken = "0123456789abcdef-0123456789abcdef"

// TestAesEncryptCBCMatchesPython 验证 Go 实现的 AES 加密与 ltpan.py 字节级一致：
// 同一段 JSON、同一 token、同一 IV，必须得到完全相同的 base64 密文。
func TestAesEncryptCBCMatchesPython(t *testing.T) {
	raw := []byte(`{"spaceType":"0","directoryId":"0","batchNo":"20260101000000","fileName":"a.txt","fileSize":123,"fileType":"4"}`)
	const want = "4FfmRJPZjxHxe/hjAH52MQy3NNq9pDEIHLMIIO4idiqTbH6QYi4Ym9pYgiQ0GtTbKm0LPHW/EpnTSWRoXxjVDQKo6F1ZxYIn8LCVHCxpyqvD1knOa07uAXJn8nLT5hbOWUSFQWdJAGwTdphsb7jDHg=="
	got, err := aesEncryptCBC(raw, testToken)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if got != want {
		t.Fatalf("密文与 ltpan.py 不一致\n got: %s\nwant: %s", got, want)
	}
	if _, err := aesEncryptCBC(raw, "short"); err == nil {
		t.Fatal("过短的 token 应当报错")
	}
}

// TestEncryptFileInfoRoundTrip 验证 encryptFileInfo 生成的密文能被正确解密，
// 且字段语义与服务端期望一致。
func TestEncryptFileInfoRoundTrip(t *testing.T) {
	obj := map[string]interface{}{
		"spaceType":   "0",
		"directoryId": "0",
		"batchNo":     "20260101000000",
		"fileName":    "电影.mkv",
		"fileSize":    int64(123456),
		"fileType":    "3",
	}
	enc, err := encryptFileInfo(obj, testToken)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	block, err := aes.NewCipher([]byte(testToken[:16]))
	if err != nil {
		t.Fatal(err)
	}
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, []byte(aesIV)).CryptBlocks(plain, raw)
	plain = plain[:len(plain)-int(plain[len(plain)-1])]

	var back map[string]interface{}
	if err := json.Unmarshal(plain, &back); err != nil {
		t.Fatalf("解密结果不是合法 JSON: %v, 明文=%s", err, plain)
	}
	if back["fileName"] != "电影.mkv" || back["fileType"] != "3" || back["directoryId"] != "0" {
		t.Fatalf("解密后字段不符: %+v", back)
	}
}

func TestExtType(t *testing.T) {
	cases := map[string]string{
		"a.JPG": "1", "b.mp3": "2", "c.mkv": "3", "d.pdf": "4",
		"e.bin": "5", "noext": "5", "x.MP4": "3", "y.docx": "4",
	}
	for name, want := range cases {
		if got := extType(name); got != want {
			t.Errorf("extType(%q) = %q, 期望 %q", name, got, want)
		}
	}
}

func TestPartSizeAndConcurrency(t *testing.T) {
	d := &Wopan2{}
	if d.partSize() != defaultPartSize {
		t.Errorf("默认分片大小 = %d, 期望 %d", d.partSize(), defaultPartSize)
	}
	if d.concurrency() != 4 {
		t.Errorf("默认并发 = %d, 期望 4", d.concurrency())
	}
	d.ChunkSize = 0
	d.Concurrency = 999
	if d.partSize() != defaultPartSize || d.concurrency() != maxWorkers {
		t.Errorf("边界处理错误: partSize=%d concurrency=%d", d.partSize(), d.concurrency())
	}
	d.ChunkSize = 1
	if d.partSize() != minPartSize {
		t.Errorf("过小的分片应提升到 %d, 实际 %d", minPartSize, d.partSize())
	}
	d.ChunkSize = 1024
	if d.partSize() != maxPartSize {
		t.Errorf("分片上限错误: %d", d.partSize())
	}
}

// TestPartLayout 验证与 ltpan.py 一致的 ceil 分片算法：
// totalPart = ceil(size/partSize)，每片长度都不超过 partSize，
// 且所有分片长度之和必须等于文件大小。
//
// 回归重点：离线下载的文件大小几乎不可能是分片大小的整数倍。
// 旧实现用 floor，20MB/16MB 会退化成单个 20MB 请求，被服务端限速。
func TestPartLayout(t *testing.T) {
	mb := int64(1 << 20)
	cases := []struct {
		name      string
		size      int64
		part      int64
		wantTotal int64
		wantLens  []int64
	}{
		{"空文件", 0, mb, 1, []int64{0}},
		{"小于一片", mb / 2, mb, 1, []int64{mb / 2}},
		{"恰好一片", mb, mb, 1, []int64{mb}},
		{"整除三片", 3 * mb, mb, 3, []int64{mb, mb, mb}},
		{"不整除", 3*mb + 12345, mb, 4, []int64{mb, mb, mb, 12345}},
		{"21MB / 8MB", 21 * mb, 8 * mb, 3, []int64{8 * mb, 8 * mb, 5 * mb}},
		{"20MB / 8MB", 20 * mb, 8 * mb, 3, []int64{8 * mb, 8 * mb, 4 * mb}},
		{"离线下载典型 20MB / 16MB", 20 * mb, 16 * mb, 2, []int64{16 * mb, 4 * mb}},
		{"离线下载典型 40MB / 16MB", 40 * mb, 16 * mb, 3, []int64{16 * mb, 16 * mb, 8 * mb}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			total, lens := partLayout(c.size, c.part)
			if total != c.wantTotal || int64(len(lens)) != c.wantTotal {
				t.Fatalf("totalPart = %d, 期望 %d", total, c.wantTotal)
			}
			var sum int64
			for i, l := range lens {
				if l != c.wantLens[i] {
					t.Errorf("分片 %d 长度 = %d, 期望 %d", i+1, l, c.wantLens[i])
				}
				sum += l
			}
			if sum != c.size {
				t.Errorf("分片总和 = %d, 期望 %d", sum, c.size)
			}
		})
	}
}

// TestUploadPartsMock 用本地 HTTP 服务验证整个上传流程：
// 分片数量/内容、multipart 表单字段、App 请求头、fileInfo 密文、并发与进度。
func TestUploadPartsMock(t *testing.T) {
	const mb = int64(8 << 20) // 与服务端要求的最小分片一致
	size := 3*mb + 12345
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 251)
	}
	tmp, err := os.CreateTemp("", "wopan2-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(content); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	parts := make(map[int64][]byte)
	form := make(map[int64]map[string]string)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openapi/client/upload2C" {
			t.Errorf("上传路径错误: %s", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got != okgoUA {
			t.Errorf("User-Agent = %q, 期望 App UA", got)
		}
		if got := r.Header.Get("Client-Id"); got != appClientID {
			t.Errorf("Client-Id = %q, 期望 %q", got, appClientID)
		}
		if got := r.Header.Get("Access-Token"); got != testToken {
			t.Errorf("Access-Token = %q", got)
		}
		if got := r.Header.Get("app-type"); got != "liantongyunpanapp" {
			t.Errorf("app-type = %q", got)
		}
		if got := r.Header.Get("Origin"); got != "https://pan.wo.cn" {
			t.Errorf("Origin = %q", got)
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Errorf("解析 multipart 失败: %v", err)
			return
		}
		idx, err := strconv.ParseInt(r.FormValue("partIndex"), 10, 64)
		if err != nil {
			t.Errorf("partIndex 非法: %v", err)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("缺少 file 分片: %v", err)
			return
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			t.Errorf("读取分片失败: %v", err)
			return
		}
		values := make(map[string]string)
		for k, v := range r.MultipartForm.Value {
			if len(v) > 0 {
				values[k] = v[0]
			}
		}
		mu.Lock()
		parts[idx] = data
		form[idx] = values
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":"0000","data":{"fid":"fid-%d"},"msg":""}`, idx)
	}))
	defer srv.Close()

	d := &Wopan2{Addition: Addition{
		AccessToken: testToken,
		UploadHost:  srv.URL,
		ChunkSize:   8,
		Concurrency: 3,
		RetryTimes:  0,
	}}
	d.deviceID = "test-device-id"
	d.uploadClient = newUploadClient()

	var progressMu sync.Mutex
	var maxProgress float64
	err = d.uploadParts(context.Background(), tmp, testToken, "movie.mkv", size, "0", func(p float64) {
		progressMu.Lock()
		if p > maxProgress {
			maxProgress = p
		}
		progressMu.Unlock()
	})
	if err != nil {
		t.Fatalf("uploadParts 失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// ceil 算法：size=3*8MB+12345, partSize=8MB -> totalPart=4，最后一片 12345 字节
	if len(parts) != 4 {
		t.Fatalf("分片数 = %d, 期望 4（ceil 分片）", len(parts))
	}
	// 按 partIndex 拼接，必须与原始内容完全一致
	var joined []byte
	for i := int64(1); i <= 4; i++ {
		joined = append(joined, parts[i]...)
	}
	if int64(len(joined)) != size {
		t.Fatalf("拼接长度 = %d, 期望 %d", len(joined), size)
	}
	for i := range joined {
		if joined[i] != content[i] {
			t.Fatalf("第 %d 字节内容不符", i)
		}
	}

	for i := int64(1); i <= 4; i++ {
		v := form[i]
		if v["totalPart"] != "4" {
			t.Errorf("分片 %d totalPart = %q", i, v["totalPart"])
		}
		if v["channel"] != "wocloud" {
			t.Errorf("分片 %d channel = %q", i, v["channel"])
		}
		if v["directoryId"] != "0" || v["fileName"] != "movie.mkv" || v["accessToken"] != testToken {
			t.Errorf("分片 %d 基础字段错误: %+v", i, v)
		}
		if v["partIndex"] != strconv.FormatInt(i, 10) {
			t.Errorf("分片 %d partIndex = %q", i, v["partIndex"])
		}
		wantLen := mb
		if i == 4 {
			wantLen = size - 3*mb // 最后一片是余数，绝不能超过 partSize
		}
		if v["partSize"] != strconv.FormatInt(wantLen, 10) {
			t.Errorf("分片 %d partSize = %q, 期望 %d", i, v["partSize"], wantLen)
		}
		if v["uniqueId"] == "" || v["fileInfo"] == "" {
			t.Errorf("分片 %d 缺少 uniqueId/fileInfo", i)
		}
		if i > 1 && v["uniqueId"] != form[1]["uniqueId"] {
			t.Errorf("分片 %d 的 uniqueId 与其他分片不一致", i)
		}
	}

	// 校验 fileInfo 密文能被解密且字段正确
	raw, err := base64.StdEncoding.DecodeString(form[1]["fileInfo"])
	if err != nil {
		t.Fatalf("fileInfo base64 错误: %v", err)
	}
	block, _ := aes.NewCipher([]byte(testToken[:16]))
	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, []byte(aesIV)).CryptBlocks(plain, raw)
	plain = plain[:len(plain)-int(plain[len(plain)-1])]
	var info map[string]interface{}
	if err := json.Unmarshal(plain, &info); err != nil {
		t.Fatalf("fileInfo 解密后不是 JSON: %v, 明文=%s", err, plain)
	}
	if info["fileName"] != "movie.mkv" || info["fileType"] != "3" || info["directoryId"] != "0" || info["spaceType"] != "0" {
		t.Fatalf("fileInfo 字段错误: %+v", info)
	}

	progressMu.Lock()
	defer progressMu.Unlock()
	if maxProgress < 99.9 {
		t.Errorf("进度未达到 100%%: %.2f", maxProgress)
	}
}

// TestE2ERealUpload 默认跳过；设置 WOPAN2_E2E_TOKEN 后会对真实的
// App 通道上传一个 1MB 测试文件，用于验证端到端连通性。
func TestE2ERealUpload(t *testing.T) {
	token := os.Getenv("WOPAN2_E2E_TOKEN")
	if token == "" {
		t.Skip("未设置 WOPAN2_E2E_TOKEN，跳过真实上传测试")
	}
	if len(token) < 16 {
		t.Fatal("token 长度不足 16 位")
	}

	const size = int64(24 << 20) // 24MB -> 3 个 8MB 分片，验证并发上传
	tmp, err := os.CreateTemp("", "wopan2-e2e-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}

	concurrency := 3
	if v := os.Getenv("WOPAN2_E2E_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			concurrency = n
		}
	}
	d := &Wopan2{Addition: Addition{
		AccessToken: token,
		UploadHost:  defaultUploadHost,
		ChunkSize:   8,
		Concurrency: concurrency,
		RetryTimes:  2,
	}}
	d.deviceID = randString(deviceIDLen, deviceCharset)
	d.uploadClient = newUploadClient()

	name := fmt.Sprintf("openlist_wopan2_test_%d.bin", time.Now().Unix())
	start := time.Now()
	err = d.uploadParts(context.Background(), tmp, token, name, size, "0", func(p float64) {
		t.Logf("进度: %.1f%%", p)
	})
	if err != nil {
		t.Fatalf("真实上传失败: %v", err)
	}
	t.Logf("真实上传成功: %s (%d 字节), 用时 %s", name, size, time.Since(start))
}
