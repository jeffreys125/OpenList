package wopan2

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestProbeSinglePartSizes 临时诊断用：测量离线下载常见文件尺寸在默认
// 16MB 分片配置下，走 App 高速节点的真实单片/多片耗时。
func TestProbeSinglePartSizes(t *testing.T) {
	token := os.Getenv("WOPAN2_E2E_TOKEN")
	if token == "" {
		t.Skip("未设置 WOPAN2_E2E_TOKEN")
	}
	chunk := int64(16)
	if v := os.Getenv("WOPAN2_PROBE_CHUNK"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n > 0 {
			chunk = n
		}
	}
	d := &Wopan2{Addition: Addition{
		AccessToken: token,
		UploadHost:  defaultUploadHost,
		ChunkSize:   chunk,
		Concurrency: 4,
		EnableQuick: false,
		RetryTimes:  0,
	}}
	t.Logf("本次探针分片大小 = %d MB", chunk)
	d.deviceID = randString(deviceIDLen, deviceCharset)
	d.uploadClient = newUploadClient()

	for _, mb := range []int64{1, 4, 20, 40} {
		size := mb << 20
		tmp, err := os.CreateTemp("", "wopan2-probe-*.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tmp.Write(make([]byte, size)); err != nil {
			t.Fatal(err)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("wopan2_probe_%dMB_%d.bin", mb, time.Now().Unix())
		start := time.Now()
		err = d.uploadParts(context.Background(), tmp, token, name, size, "0", func(float64) {})
		elapsed := time.Since(start)
		t.Logf("【探针】%2dMB: 耗时 %-12s 平均 %6.2f MB/s  err=%v",
			mb, elapsed.Round(time.Millisecond), float64(size)/elapsed.Seconds()/1024/1024, err)
		tmp.Close()
		os.Remove(tmp.Name())
	}
}
