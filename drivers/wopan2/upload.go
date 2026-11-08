package wopan2

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/wopan-sdk-go"
	"github.com/go-resty/resty/v2"
	"golang.org/x/sync/errgroup"
)

// App 通道常量，全部来自 ltpan.py 的实测配置。
// 关键差异：上传节点是 hyupload.pan.wo.cn（App 专用、不限速），
// 而不是 SDK 使用的 tjupload.pan.wo.cn / GetZoneInfo 网页节点。
const (
	defaultUploadHost   = "https://hyupload.pan.wo.cn"
	defaultTransferHost = "https://b.smartont.net"

	appClientID = "1001000035"
	appVersion  = "yp-app/6.5.0"
	okgoUA      = "okhttp-okgo/jeasonlzy LianTongYunPan/6.5.0 (Android 16)"
	bundleID    = "com.chinaunicom.bol.cloudapp"

	// AES-CBC 固定 IV；key = accessToken[:16]，与 SDK / ltpan.py 完全一致。
	aesIV = "wNSOYIB1k1DjY5lA"

	defaultPartSize = int64(16 << 20)
	// 服务端对分片大小有下限：实测 1MB / 4MB 分片在合并最后一片时返回
	// HTTP 500，8MB 及以上分片（整除、不整除、多片）均可正常合并。
	// 与 wopan-sdk-go 的 DefaultPartSize(8MB) 保持一致。
	minPartSize = int64(8 << 20)
	maxPartSize = int64(128 << 20)
	maxWorkers  = 16

	uploadTimeout = 15 * time.Minute
	deviceIDLen   = 32
)

const (
	deviceCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/+"
	letterCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

func randString(n int, charset string) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

func randHexUpper(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(rand.Intn(256))
		}
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

// newUploadClient 建立独立于 dispatcher 的 HTTP 客户端：
// 更大的连接池（并发分片需要）+ 长超时（大分片需要），且 UA / header 互不干扰。
func newUploadClient() *resty.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	hc := &http.Client{Transport: transport, Timeout: uploadTimeout}
	return resty.NewWithClient(hc)
}

// aesEncryptCBC 复刻 ltpan.py 的 WoPan.encrypt：
// JSON -> PKCS7 padding -> AES-CBC(key=token[:16], iv=aesIV) -> base64。
// 自己实现而不是调用 SDK 的 EncryptParam，是为了让整个上传流程只用
// 一份 token 快照（避免上传途中 token 刷新导致 key 与表单 token 不一致）。
func aesEncryptCBC(raw []byte, token string) (string, error) {
	if len(token) < 16 {
		return "", fmt.Errorf("access token 至少需要 16 位，当前 %d 位", len(token))
	}
	block, err := aes.NewCipher([]byte(token[:16]))
	if err != nil {
		return "", err
	}
	pad := aes.BlockSize - len(raw)%aes.BlockSize
	padded := make([]byte, 0, len(raw)+pad)
	padded = append(padded, raw...)
	padded = append(padded, bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(aesIV)).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

func encryptFileInfo(obj interface{}, token string) (string, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return aesEncryptCBC(raw, token)
}

// appHeaders 复刻 ltpan.py 的 headers("upload")（App 专属头）。
// 注意 User-Agent 不放在这里，而是在每个请求上单独 SetHeader 覆盖，
// 以免与 dispatcher 的浏览器 UA 混在一起。
func (d *Wopan2) appHeaders(token string) map[string]string {
	return map[string]string{
		"X-YP-Device-Id":  d.deviceID,
		"Access-Token":    token,
		"accesstoken":     token,
		"Client-Id":       appClientID,
		"App-Version":     appVersion,
		"appversion":      "6.5.0",
		"Sys-Version":     "Android/16",
		"sys-version":     "Android/16",
		"platform":        "1",
		"platfomr":        "1",
		"app-type":        "liantongyunpanapp",
		"bundleid":        bundleID,
		"appchannel":      "other",
		"network-type":    "wifi",
		"oaid":            "00000000",
		"Accept-Language": "zh-CN,zh;q=0.8",
		"Origin":          "https://pan.wo.cn",
		"Referer":         "https://pan.wo.cn/",
	}
}

func cleanHost(h, fallback string) string {
	h = strings.TrimRight(strings.TrimSpace(h), "/")
	if h == "" {
		return fallback
	}
	return h
}

func (d *Wopan2) partSize() int64 {
	if d.ChunkSize <= 0 {
		return defaultPartSize
	}
	ps := d.ChunkSize << 20
	if ps < minPartSize {
		return minPartSize
	}
	if ps > maxPartSize {
		return maxPartSize
	}
	return ps
}

func (d *Wopan2) concurrency() int {
	if d.Concurrency <= 0 {
		return 4
	}
	if d.Concurrency > maxWorkers {
		return maxWorkers
	}
	return d.Concurrency
}

func (d *Wopan2) retryTimes() int {
	if d.RetryTimes < 0 {
		return 0
	}
	if d.RetryTimes > 5 {
		return 5
	}
	return d.RetryTimes
}

// ---------------------------------------------------------------- 秒传

type quickTransferResp struct {
	Result struct {
		HasFile interface{} `json:"hasFile"`
		FileID  string      `json:"fileId"`
	} `json:"result"`
}

func (d *Wopan2) quickTransfer(ctx context.Context, token, name string, size int64, sha, dirID string, mtime time.Time) (string, error) {
	host := cleanHost(d.TransferHost, defaultTransferHost)
	if mtime.IsZero() {
		mtime = time.Now()
	}
	body := map[string]interface{}{
		"batchNo":              randHexUpper(16),
		"deviceId":             "",
		"directoryId":          dirID,
		"familyId":             0,
		"fileModificationTime": mtime.UnixMilli(),
		"fileName":             name,
		"fileSize":             strconv.FormatInt(size, 10),
		"fileType":             extType(name),
		"height":               "",
		"lat":                  "",
		"lng":                  "",
		"psToken":              "",
		"sha256":               sha,
		"spaceType":            d.getSpaceType(),
		"width":                "",
	}
	var res quickTransferResp
	req := d.uploadClient.R().
		SetHeaders(d.appHeaders(token)).
		SetHeader("User-Agent", okgoUA).
		SetHeader("client-Id", appClientID).
		SetHeader("app-version", appVersion).
		SetHeader("access-token", token).
		SetBody(body).
		SetResult(&res).
		SetContext(ctx)
	r, err := req.Post(host + "/openapi/transfer/quickTransfer")
	if err != nil {
		return "", err
	}
	if r.IsError() {
		return "", fmt.Errorf("秒传接口返回 HTTP %d: %s", r.StatusCode(), r.String())
	}
	if res.Result.FileID != "" && fmt.Sprint(res.Result.HasFile) == "1" {
		return res.Result.FileID, nil
	}
	return "", nil
}

func sha256Of(f model.File, size int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, size)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------- 上传

type uploadResp struct {
	Code string `json:"code"`
	Data struct {
		FID string `json:"fid"`
	} `json:"data"`
	Msg string `json:"msg"`
}

// fastUpload 是 Put 的实现：
//  1. 把流式 FileStreamer 缓存成可随机读的 model.File（并发分片 / SHA256 需要）；
//  2. 可选秒传（命中则直接返回）；
//  3. 走 hyupload.pan.wo.cn 的 App 通道做多分片并发上传。
func (d *Wopan2) fastUpload(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	name := stream.GetName()
	size := stream.GetSize()
	if size < 0 {
		size = 0
	}
	token := d.AccessToken
	if len(token) < 16 {
		return fmt.Errorf("access token 无效：至少需要 16 位")
	}

	cacheUp := model.UpdateProgressWithRange(up, 0, 10)
	cached, err := stream.CacheFullAndWriter(&cacheUp, nil)
	if err != nil {
		return fmt.Errorf("缓存上传流失败: %w", err)
	}
	if cached == nil {
		return errors.New("缓存上传流失败：返回空文件")
	}
	upPart := model.UpdateProgressWithRange(up, 10, 100)

	if d.EnableQuick && size > 0 {
		if sha, e := sha256Of(cached, size); e == nil {
			if fid, e := d.quickTransfer(ctx, token, name, size, sha, dstDir.GetID(), stream.ModTime()); e == nil && fid != "" {
				upPart(100)
				return nil
			}
		}
	}

	return d.uploadParts(ctx, cached, token, name, size, dstDir.GetID(), upPart)
}

// partLayout 按「SDK 风格」计算分片（与 wopan-sdk-go 的 Upload2C 一致）：
//   - totalPart = size / partSize（整除向下）；
//   - 前 totalPart-1 片各 partSize，最后一片吃掉全部剩余（可大于 partSize）。
//
// 关键实测结论：服务端要求分片不小于约 8MB，过小的分片会让合并最后一片时
// 返回 HTTP 500。partSize() 会把低于 minPartSize 的配置自动提升，因此这里
// 拿到的 partSize 一定是安全的；在安全分片大小下，整除 / 不整除、2 片 / 3 片
// 都能正常合并并返回 fid。
func partLayout(size, partSize int64) (int64, []int64) {
	if partSize <= 0 {
		partSize = defaultPartSize
	}
	total := size / partSize
	if total == 0 {
		total = 1
	}
	lengths := make([]int64, total)
	for i := int64(1); i <= total; i++ {
		lengths[i-1] = partSize
		if i == total {
			lengths[i-1] = size - (i-1)*partSize
		}
		if lengths[i-1] < 0 {
			lengths[i-1] = 0
		}
	}
	return total, lengths
}

func (d *Wopan2) uploadParts(ctx context.Context, f model.File, token, name string, size int64, dirID string, up driver.UpdateProgress) error {
	partSize := d.partSize()
	totalPart, lengths := partLayout(size, partSize)

	batchNo := time.Now().Format("20060102150405")
	uniqueID := strconv.FormatInt(time.Now().UnixMilli(), 10) + "_" + randString(6, letterCharset)

	fileInfo := map[string]interface{}{
		"spaceType":   d.getSpaceType(),
		"directoryId": dirID,
		"batchNo":     batchNo,
		"fileName":    name,
		"fileSize":    size,
		"fileType":    extType(name),
	}
	if d.getSpaceType() == wopan.SpaceTypeFamily {
		fileInfo["familyId"] = d.FamilyID
	}
	fileInfoStr, err := encryptFileInfo(fileInfo, token)
	if err != nil {
		return fmt.Errorf("加密 fileInfo 失败: %w", err)
	}

	common := map[string]string{
		"uniqueId":    uniqueID,
		"accessToken": token,
		"fileName":    name,
		"psToken":     "undefined",
		"fileSize":    strconv.FormatInt(size, 10),
		"totalPart":   strconv.FormatInt(totalPart, 10),
		"channel":     "wocloud",
		"directoryId": dirID,
		"fileInfo":    fileInfoStr,
	}
	url := cleanHost(d.UploadHost, defaultUploadHost) + "/openapi/client/upload2C"

	var done int64
	var fidMu sync.Mutex
	var fid string

	sendOne := func(c context.Context, idx int64) error {
		offset := (idx - 1) * partSize
		length := lengths[idx-1]
		var lastErr error
		for attempt := 0; attempt <= d.retryTimes(); attempt++ {
			if err := c.Err(); err != nil {
				return err
			}
			gotFID, err := d.sendPart(c, url, common, f, token, name, idx, offset, length)
			if err == nil {
				if gotFID != "" {
					fidMu.Lock()
					if fid == "" {
						fid = gotFID
					}
					fidMu.Unlock()
				}
				cur := atomic.AddInt64(&done, length)
				if size > 0 {
					up(float64(cur) * 100 / float64(size))
				}
				return nil
			}
			lastErr = err
			if attempt < d.retryTimes() {
				select {
				case <-c.Done():
					return c.Err()
				case <-time.After(time.Duration(attempt+1) * time.Second):
				}
			}
		}
		return fmt.Errorf("分片 %d/%d 上传失败: %w", idx, totalPart, lastErr)
	}

	workers := d.concurrency()
	if int64(workers) > totalPart {
		workers = int(totalPart)
	}

	// 前 totalPart-1 片并发上传，提升吞吐。
	if totalPart > 1 {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(workers)
		for i := int64(1); i < totalPart; i++ {
			idx := i
			g.Go(func() error { return sendOne(gctx, idx) })
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}

	// 最后一片（partIndex == totalPart）会触发服务端合并，必须等前序分片
	// 全部成功后再单独发送，否则合并时数据不齐会返回 500。
	if err := sendOne(ctx, totalPart); err != nil {
		return err
	}

	if size == 0 {
		up(100)
	}
	// 服务端按 batchNo + partIndex 自行合并，返回的 fid 仅用于记录。
	_ = fid
	return nil
}

func (d *Wopan2) sendPart(ctx context.Context, url string, common map[string]string, f model.File, token, name string, idx, offset, length int64) (string, error) {
	form := make(map[string]string, len(common)+2)
	for k, v := range common {
		form[k] = v
	}
	form["partIndex"] = strconv.FormatInt(idx, 10)
	form["partSize"] = strconv.FormatInt(length, 10)

	partCtx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	section := io.NewSectionReader(f, offset, length)
	limited := driver.NewLimitedUploadStream(partCtx, section)

	var resp uploadResp
	req := d.uploadClient.R().
		SetHeaders(d.appHeaders(token)).
		SetHeader("User-Agent", okgoUA).
		SetMultipartFormData(form).
		SetMultipartField("file", name, "application/octet-stream", limited).
		ForceContentType("application/json;charset=UTF-8").
		SetResult(&resp).
		SetContext(partCtx)
	r, err := req.Post(url)
	if err != nil {
		return "", err
	}
	if r.IsError() {
		return "", fmt.Errorf("HTTP %d: %s", r.StatusCode(), r.String())
	}
	if resp.Code != "0000" {
		return "", fmt.Errorf("code=%s msg=%s", resp.Code, resp.Msg)
	}
	return resp.Data.FID, nil
}
