package wopan2

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

// TestCleanupE2EFiles 清理历次实测在网盘里留下的测试文件。默认跳过，
// 仅在显式设置 WOPAN2_E2E_TOKEN 时运行；只删除本仓库测试脚本生成的固定前缀，
// 不会碰用户自己的文件。
func TestCleanupE2EFiles(t *testing.T) {
	token := os.Getenv("WOPAN2_E2E_TOKEN")
	if token == "" {
		t.Skip("未设置 WOPAN2_E2E_TOKEN")
	}
	d := &Wopan2{Addition: Addition{AccessToken: token}}
	d.deviceID = randString(deviceIDLen, deviceCharset)
	d.uploadClient = newUploadClient()
	if err := d.Init(context.Background()); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	prefixes := []string{
		"openlist_wopan2_test_", "openlist_wopan2_fullput_", "wopan2_probe_", "probe_",
		"wopan2_ab_", "wopan2_webchannel_", "wopan2_web_", "wopan2_diag_", "ltpan_ab_",
	}
	ctx := context.Background()
	objs, err := d.List(ctx, &model.Object{ID: "0"}, model.ListArgs{})
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	deleted := 0
	for _, o := range objs {
		match := false
		for _, p := range prefixes {
			if strings.HasPrefix(o.GetName(), p) {
				match = true
				break
			}
		}
		if !match || o.IsDir() {
			continue
		}
		if err := d.Remove(ctx, o); err != nil {
			t.Errorf("删除 %s 失败: %v", o.GetName(), err)
		} else {
			deleted++
		}
	}
	t.Logf("共清理 %d 个测试文件，目录里还有 %d 个对象", deleted, len(objs)-deleted)
}
