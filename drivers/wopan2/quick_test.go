package wopan2

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
)

// TestQuickTransferMustNotBlockUpload 是一个回归测试，复现「离线下载上传只有几十 K」的根因：
//
// 秒传节点（b.smartont.net）在部分网络下响应极慢或不可达。修复前 fastUpload 会
// 用一个 15 分钟超时的 HTTP 客户端去查秒传，且失败被静默吞掉，导致整个 Put 在
// 上传开始前先卡住几十秒到几分钟，UI 上按「总耗时」算出的平均速度就只剩几十 K。
//
// 期望：秒传查询必须带短超时并快速降级，上传不应被它阻塞。
func TestQuickTransferMustNotBlockUpload(t *testing.T) {
	var uploadCalls atomic.Int64
	var firstUploadAt atomic.Int64
	stop := make(chan struct{})

	transfer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟节点无响应；测试结束时主动放行，避免拖慢收尾。
		select {
		case <-stop:
			return
		case <-time.After(20 * time.Second):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"hasFile":0,"fileId":""}}`))
	}))
	defer func() {
		close(stop)
		transfer.CloseClientConnections()
		transfer.Close()
	}()

	upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstUploadAt.CompareAndSwap(0, time.Now().UnixMilli())
		uploadCalls.Add(1)
		_ = r.ParseMultipartForm(32 << 20)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"0000","data":{"fid":"mock-fid"},"msg":""}`))
	}))
	defer upload.Close()

	const size = int64(1 << 20)
	tmp, err := os.CreateTemp("", "wopan2-quick-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	// *os.File 实现 model.File，CacheFullAndWriter 会零拷贝直接返回。
	fs := &stream.FileStream{
		Obj:      &model.Object{Name: "quick-block-test.bin", Size: size},
		Reader:   tmp,
		Mimetype: "application/octet-stream",
		Ctx:      context.Background(),
	}

	d := &Wopan2{Addition: Addition{
		AccessToken:  testToken,
		UploadHost:   upload.URL,
		TransferHost: transfer.URL,
		ChunkSize:    8,
		Concurrency:  1,
		EnableQuick:  true, // 显式打开，验证即使开启也不能阻塞上传
		RetryTimes:   0,
	}}
	d.deviceID = "test-device"
	d.uploadClient = newUploadClient()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	err = d.fastUpload(ctx, &model.Object{ID: "0"}, fs, func(float64) {})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("慢秒传节点不应导致上传失败: %v (耗时 %s)", err, elapsed)
	}
	if elapsed > 12*time.Second {
		t.Fatalf("上传被秒传查询阻塞: 耗时 %s", elapsed)
	}
	if uploadCalls.Load() == 0 {
		t.Fatal("没有发起任何上传请求")
	}
	delay := time.Duration(firstUploadAt.Load()-start.UnixMilli()) * time.Millisecond
	t.Logf("上传在 %.2fs 后开始，总耗时 %.2fs", delay.Seconds(), elapsed.Seconds())
	if delay > 10*time.Second {
		t.Fatalf("上传开始得太晚: %.2fs", delay.Seconds())
	}
}

// TestE2EFullPut 默认跳过。与 TestE2ERealUpload 不同，它走完整的
// Put -> fastUpload 链路（含 CacheFullAndWriter、可选的秒传降级、分片并发上传），
// 用来验证修复后「离线下载」场景的端到端吞吐确实落在高速通道上。
func TestE2EFullPut(t *testing.T) {
	token := os.Getenv("WOPAN2_E2E_TOKEN")
	if token == "" {
		t.Skip("未设置 WOPAN2_E2E_TOKEN，跳过完整 Put 端到端测试")
	}

	size := int64(64 << 20) // 64MB -> 8 个 8MB 分片
	if v := os.Getenv("WOPAN2_E2E_SIZE_MB"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n > 0 {
			size = n << 20
		}
	}
	concurrency := 4
	if v := os.Getenv("WOPAN2_E2E_CONCURRENCY"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			concurrency = n
		}
	}
	tmp, err := os.CreateTemp("", "wopan2-fullput-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	name := fmt.Sprintf("openlist_wopan2_fullput_%d.bin", time.Now().Unix())
	fs := &stream.FileStream{
		Obj:      &model.Object{Name: name, Size: size},
		Reader:   tmp,
		Mimetype: "application/octet-stream",
		Ctx:      context.Background(),
	}

	// 与用户实际部署的默认配置一致：秒传关闭、8MB 分片、4 并发。
	d := &Wopan2{Addition: Addition{
		AccessToken:  token,
		UploadHost:   defaultUploadHost,
		TransferHost: defaultTransferHost,
		ChunkSize:    8,
		Concurrency:  concurrency,
		EnableQuick:  false,
		RetryTimes:   2,
	}}
	d.deviceID = randString(deviceIDLen, deviceCharset)
	d.uploadClient = newUploadClient()

	var last float64
	start := time.Now()
	err = d.fastUpload(context.Background(), &model.Object{ID: "0"}, fs, func(p float64) {
		last = p
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("完整 Put 失败: %v", err)
	}
	if last < 99.9 {
		t.Fatalf("进度未达到 100%%: %.1f", last)
	}
	t.Logf("完整 Put 成功: %s (%d 字节), 耗时 %s, 平均 %.2f MB/s",
		name, size, elapsed, float64(size)/elapsed.Seconds()/1024/1024)
}
