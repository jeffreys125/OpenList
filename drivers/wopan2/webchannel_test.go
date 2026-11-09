package wopan2

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	wopan "github.com/OpenListTeam/wopan-sdk-go"
)

// TestABWebChannel 在同一时刻用 SDK 的网页通道（tjupload.pan.wo.cn，
// Client-Id 1001000021 + 浏览器 UA + 8MB 串行分片）上传同样大小，
// 与 App 通道做对照，判断是「账号/App 节点被限流」还是「代码问题」。
func TestABWebChannel(t *testing.T) {
	token := os.Getenv("WOPAN2_E2E_TOKEN")
	if token == "" {
		t.Skip("未设置 WOPAN2_E2E_TOKEN")
	}
	const size = int64(21 << 20)
	tmp, err := os.CreateTemp("", "wopan2-web-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		t.Fatal(err)
	}

	c := wopan.Default()
	c.SetAccessToken(token)
	c.SetUserAgent(wopan.DefaultUA)
	if err := c.InitData(); err != nil {
		t.Logf("InitData 失败（将回退到默认 tjupload 节点）: %v", err)
	} else {
		t.Logf("分区节点 ZoneURL = %s", c.ZoneURL)
	}

	name := fmt.Sprintf("wopan2_webchannel_%d.bin", time.Now().Unix())
	start := time.Now()
	fid, err := c.Upload2CPersonal(wopan.Upload2CFile{
		Name:        name,
		Size:        size,
		Content:     tmp,
		ContentType: "application/octet-stream",
	}, "0", wopan.Upload2COption{
		Ctx: context.Background(),
		OnProgress: func(cur, total int64) {
			t.Logf("网页通道进度: %.1f%%", float64(cur)*100/float64(total))
		},
	})
	elapsed := time.Since(start)
	t.Logf("【网页通道】耗时 %s, 吞吐 %.2f MB/s, fid=%s, err=%v",
		elapsed.Round(time.Millisecond), float64(size)/elapsed.Seconds()/1024/1024, fid, err)
}
