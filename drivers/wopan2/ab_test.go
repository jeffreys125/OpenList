package wopan2

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// TestABOfflineDownloadChain 完全复刻离线下载 transferStdFile 传入的
// FileStream（os.Open 的 *os.File + Closers），用真实网盘对比：
//   - 分片 8MB / 16MB
//   - up 回调为 no-op / 模拟 tache.Persist 开销
//
// 用来区分「真实网络吞吐慢」「高频进度持久化拖慢」「分片配置影响」。
func TestABOfflineDownloadChain(t *testing.T) {
	token := os.Getenv("WOPAN2_E2E_TOKEN")
	if token == "" {
		t.Skip("未设置 WOPAN2_E2E_TOKEN")
	}
	sizeMB := int64(21) // 非 8/16 整数倍，离线下载典型
	if v := os.Getenv("WOPAN2_AB_SIZE_MB"); v != "" {
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n > 0 {
			sizeMB = n
		}
	}
	size := sizeMB << 20

	type caseDef struct {
		name       string
		chunk      int64
		simPersist bool
	}
	cases := []caseDef{
		{"8MB分片/no-op进度", 8, false},
		{"16MB分片/no-op进度", 16, false},
		{"8MB分片/模拟Persist", 8, true},
		{"16MB分片/模拟Persist", 16, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp, err := os.CreateTemp("", "wopan2-ab-*.bin")
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

			// 与 transferStdFile 完全一致
			fs := &stream.FileStream{
				Ctx: context.Background(),
				Obj: &model.Object{
					Name:     fmt.Sprintf("wopan2_ab_%dMB_%d.bin", sizeMB, time.Now().Unix()),
					Size:     size,
					Modified: time.Now(),
					IsFolder: false,
				},
				Reader:   tmp,
				Mimetype: "application/octet-stream",
				Closers:  utils.NewClosers(tmp),
			}
			t.Logf("GetFile() 可直接复用(不缓存) = %v", fs.GetFile() != nil)

			host := os.Getenv("WOPAN2_AB_HOST")
			if host == "" {
				host = defaultUploadHost
			}
			t.Logf("本次上传节点 = %s", host)
			d := &Wopan2{Addition: Addition{
				AccessToken:  token,
				UploadHost:   host,
				TransferHost: defaultTransferHost,
				ChunkSize:    c.chunk,
				Concurrency:  4,
				EnableQuick:  false,
				RetryTimes:   0,
			}}
			d.deviceID = randString(deviceIDLen, deviceCharset)
			d.uploadClient = newUploadClient()

			var calls atomic.Int64
			up := func(p float64) {
				calls.Add(1)
				if c.simPersist {
					// tache.Base.SetProgress 每次都会 Persist() 写库，
					// 这里用 1ms 模拟一次持久化开销的下限。
					time.Sleep(time.Millisecond)
				}
			}
			start := time.Now()
			err = d.Put(context.Background(), &model.Object{ID: "0"}, fs, up)
			elapsed := time.Since(start)
			t.Logf("【AB】%s: 耗时 %s, 吞吐 %.2f MB/s, up调用 %d 次, err=%v",
				c.name, elapsed.Round(time.Millisecond),
				float64(size)/elapsed.Seconds()/1024/1024, calls.Load(), err)
		})
	}
}
