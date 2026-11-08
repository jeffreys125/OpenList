package wopan2

import (
	"path"
	"strings"
	"time"

	"github.com/OpenListTeam/wopan-sdk-go"
)

func (d *Wopan2) getSortRule() int {
	switch d.SortRule {
	case "name_asc":
		return wopan.SortNameAsc
	case "name_desc":
		return wopan.SortNameDesc
	case "time_asc":
		return wopan.SortTimeAsc
	case "time_desc":
		return wopan.SortTimeDesc
	case "size_asc":
		return wopan.SortSizeAsc
	case "size_desc":
		return wopan.SortSizeDesc
	default:
		return wopan.SortNameAsc
	}
}

func (d *Wopan2) getSpaceType() string {
	if d.FamilyID == "" {
		return wopan.SpaceTypePersonal
	}
	return wopan.SpaceTypeFamily
}

// 20230607214351
func getTime(str string) (time.Time, error) {
	loc := time.FixedZone("UTC+8", 8*60*60)
	return time.ParseInLocation("20060102150405", str, loc)
}

var (
	imageExts = newSet("jpg", "jpeg", "png", "gif", "bmp", "webp", "heic")
	audioExts = newSet("mp3", "wav", "flac", "aac", "m4a", "ogg")
	videoExts = newSet("mp4", "mkv", "avi", "mov", "flv", "ts", "rmvb", "wmv")
	docExts   = newSet("doc", "docx", "xls", "xlsx", "ppt", "pptx", "pdf", "txt", "md", "csv")
)

func newSet(items ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(items))
	for _, it := range items {
		m[it] = struct{}{}
	}
	return m
}

// extType 与 ltpan.py 的 ext_type 完全一致：
// 1 图片 2 音频 3 视频 4 文档 5 其它（传 5 服务端也会自行纠正）。
// 用本地静态映射，避免 SDK 的 GetFileType 额外发一次 ClassifyRule 请求。
func extType(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	switch {
	case has(imageExts, ext):
		return "1"
	case has(audioExts, ext):
		return "2"
	case has(videoExts, ext):
		return "3"
	case has(docExts, ext):
		return "4"
	default:
		return "5"
	}
}

func has(m map[string]struct{}, k string) bool {
	_, ok := m[k]
	return ok
}
