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
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/wopan-sdk-go"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// App 通道常量，全部来自 ltpan.py 的实测配置。
// 与原驱动的关键差异是 headers / Client-Id 与并发分片策略，API 相同；
// 上传节点的选择才是吞吐的决定因素，见下方 defaultUploadHost 说明。
// defaultUploadHost 经过实测后改为 tjupload.pan.wo.cn。
// 同一 token、同一 App headers、同一 upload2C 接口、同一 21MB 文件：
//
//	hyupload.pan.wo.cn  -> 0.26~0.36 MB/s（该节点被服务端限速，与账号、分片、并发无关）
//	tjupload.pan.wo.cn  -> 5.0~7.0 MB/s
//
// 注意 SDK 的 GetZoneInfo 会动态返回 hyupload，所以原「联通网盘」驱动的网页
// 通道实际也打在被限速的节点上；绕开分区调度、直接指定 tjupload 才能稳定高速。
// 若你的网络下某个节点更快，可在管理后台用 upload_host 覆盖（见 diagnose.py）。
const (
	defaultUploadHost   = "https://tjupload.pan.wo.cn"
	defaultTransferHost = "https://b.smartont.net"

	appClientID = "1001000035"
	appVersion  = "yp-app/6.5.0"
	okgoUA      = "okhttp-okgo/jeasonlzy LianTongYunPan/6.5.0 (Android 16)"
	bundleID    = "com.chinaunicom.bol.cloudapp"

	// AES-CBC 固定 IV；key = accessToken[:16]，与 SDK / ltpan.py 完全一致。
	aesIV = "wNSOYIB1k1DjY5lA"

	// 默认 8MB：与 SDK / 原驱动保持一致，也是服务端合并分片的安全下限。
	defaultPartSize = int64(8 << 20)
	// 服务端对分片大小有下限：实测 1MB / 4MB 分片在合并最后一片时返回
	// HTTP 500，8MB 及以上分片（整除、不整除、多片）均可正常合并。
	minPartSize = int64(8 << 20)
	maxPartSize = int64(128 << 20)
	maxWorkers  = 16

	uploadTimeout = 15 * time.Minute
	deviceIDLen   = 32

	// 秒传查询是「控制类」请求，命中率本身很低（尤其离线下载的 URL 资源），
	// 却位于上传链路的最前面。它必须快速失败并降级，绝不能拖住整个 Put。
	// 修复前该请求与分片上传共用 15 分钟超时的客户端，节点不可达时
	// 会让上传在开始前白等几十秒到几分钟，UI 上的平均速度被拉到几十 K。
	quickTransferTimeout = 6 * time.Second
	dialTimeout          = 8 * time.Second
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
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
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
//  3. 走 App 通道（默认 tjupload.pan.wo.cn）做多分片并发上传。
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

	// 离线下载传入的 FileStreamer，其底层 Reader 往往已经是 *os.File
	// （本地临时文件），或者是一个远程 HTTP Range 流。前者应当直接复用、
	// 不做任何额外复制；后者才需要缓存成可随机读的 model.File。
	// 无条件调用 CacheFullAndWriter 会在上传开始前多出一次完整读/复制，
	// 这段时间不计入上传吞吐，却计入任务耗时，把 UI 平均速度拉低。
	var cached model.File
	upPart := up
	if f := stream.GetFile(); f != nil {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("重置上传流失败: %w", err)
		}
		cached = f
	} else {
		cacheUp := model.UpdateProgressWithRange(up, 0, 10)
		var err error
		cached, err = stream.CacheFullAndWriter(&cacheUp, nil)
		if err != nil {
			return fmt.Errorf("缓存上传流失败: %w", err)
		}
		if cached == nil {
			return errors.New("缓存上传流失败：返回空文件")
		}
		upPart = model.UpdateProgressWithRange(up, 10, 100)
	}

	// 秒传默认关闭：离线下载的 URL 资源几乎不可能已在网盘中存在，
	// 但每次开启都要额外完整扫一遍文件算 SHA256 + 一次跨网请求，纯属前置开销。
	// 即便手动开启，也必须使用独立的短超时上下文：b.smartont.net 在部分网络下
	// 响应极慢或不可达，修复前它与分片上传共用 15 分钟超时客户端且失败被静默吞掉，
	// 导致整个 Put 在上传开始前白等几十秒到几分钟（UI 上表现为「几十 K」）。
	if d.EnableQuick && size > 0 {
		quickCtx, cancel := context.WithTimeout(ctx, quickTransferTimeout)
		defer cancel()
		sha, e := sha256Of(cached, size)
		switch {
		case e != nil:
			log.Warnf("[联通网盘2] 计算 SHA256 失败，跳过秒传直接上传: %v", e)
		default:
			fid, e := d.quickTransfer(quickCtx, token, name, size, sha, dstDir.GetID(), stream.ModTime())
			switch {
			case e != nil:
				log.Warnf("[联通网盘2] 秒传查询在 %s 内未完成（节点不可达或超时），已跳过秒传直接上传: %v", quickTransferTimeout, e)
			case fid != "":
				upPart(100)
				return nil
			}
		}
	}

	return d.uploadParts(ctx, cached, token, name, size, dstDir.GetID(), upPart)
}

// partLayout 严格复刻 ltpan.py 的分片算法：
//   - totalPart = ceil(size / partSize)；
//   - 每片长度 min(partSize, size-offset)，即任何一片都不会超过 partSize。
//
// 早期版本错误地照搬 wopan-sdk-go 的 floor 算法（最后一片 = 余数 + partSize，
// 最大可达 2*partSize），与 ltpan.py 不一致，并会产生超过分片大小的请求体。
// 现已与 Python 程序字节级一致。
// 注意：上传吞吐的主要决定因素是「上传节点」而非分片算法，见 defaultUploadHost。
func partLayout(size, partSize int64) (int64, []int64) {
	if partSize <= 0 {
		partSize = defaultPartSize
	}
	if size <= 0 {
		return 1, []int64{0}
	}
	total := (size + partSize - 1) / partSize
	lengths := make([]int64, total)
	for i := int64(0); i < total; i++ {
		length := partSize
		if remain := size - i*partSize; remain < length {
			length = remain
		}
		lengths[i] = length
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

	var sent int64
	var fidMu sync.Mutex
	var fid string
	start := time.Now()

	// 按实际发出去的字节上报，但必须节流：
	// up 最终落到 tache.Base.SetProgress，后者每次调用都会 Persist() 写库。
	// resty 发送 multipart 时每 32KB 左右读一次，若不节流，一个 8MB 分片
	// 就会产生 250+ 次数据库写，在任务多或磁盘慢的机器上会明显拖慢上传。
	// 这里按「时间 >=250ms 或进度 >=1%」双重节流，收尾处强制补 100%。
	var reportMu sync.Mutex
	var lastReport time.Time
	var lastPct float64
	report := func(n int64) {
		if size <= 0 || n <= 0 {
			return
		}
		cur := atomic.AddInt64(&sent, n)
		p := float64(cur) * 100 / float64(size)
		if p > 100 {
			p = 100 // 分片重传会让 sent 暂时超过 size，收尾时会强制校正
		}
		reportMu.Lock()
		defer reportMu.Unlock()
		now := time.Now()
		if p < 100 && now.Sub(lastReport) < 250*time.Millisecond && p-lastPct < 1 {
			return
		}
		lastReport, lastPct = now, p
		up(p)
	}

	sendOne := func(c context.Context, idx int64) error {
		offset := (idx - 1) * partSize
		length := lengths[idx-1]
		var lastErr error
		for attempt := 0; attempt <= d.retryTimes(); attempt++ {
			if err := c.Err(); err != nil {
				return err
			}
			partStart := time.Now()
			gotFID, err := d.sendPart(c, url, common, f, token, name, idx, offset, length, report)
			if os.Getenv("WOPAN2_DEBUG_PARTS") != "" {
				log.Warnf("[联通网盘2] 分片 %d/%d 第 %d 次尝试: 长度 %d 字节, 耗时 %s, err=%v",
					idx, totalPart, attempt+1, length, time.Since(partStart), err)
			}
			if err == nil {
				if gotFID != "" {
					fidMu.Lock()
					if fid == "" {
						fid = gotFID
					}
					fidMu.Unlock()
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

	// 强制校正进度（分片重传可能让 sent 暂时超过真实大小）。
	up(100)
	// 服务端按 batchNo + partIndex 自行合并，返回的 fid 仅用于记录。
	_ = fid

	elapsed := time.Since(start)
	var rateMB float64
	if elapsed > 0 && size > 0 {
		rateMB = float64(size) / elapsed.Seconds() / 1024 / 1024
	}
	host := cleanHost(d.UploadHost, defaultUploadHost)
	log.Infof("[联通网盘2] 上传完成: %d 片, %d 字节, 耗时 %s, 实测 %.2f MB/s | 生效配置: 节点=%s 分片=%dMB 并发=%d 秒传=%v 重试=%d",
		totalPart, size, elapsed.Round(time.Millisecond), rateMB, host, partSize>>20, workers, d.EnableQuick, d.retryTimes())
	if rateMB < 1.5 {
		// 用 Warnf 保证即使日志级别设为 warn 也能看到这条关键结论。
		log.Warnf("[联通网盘2] 实测吞吐仅 %.2f MB/s：当前节点 %s 被服务端限速（换 token、改分片/并发/秒传都无效）。请在管理后台把 upload_host 改为 https://tjupload.pan.wo.cn，或用随附的 diagnose.py 在你的服务器上测出最快节点后填入", rateMB, host)
	}
	return nil
}

// countingReader 按实际从分片读取的字节数实时触发进度回调。
type countingReader struct {
	r      io.Reader
	onRead func(int64)
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && r.onRead != nil {
		r.onRead(int64(n))
	}
	return n, err
}

func (d *Wopan2) sendPart(ctx context.Context, url string, common map[string]string, f model.File, token, name string, idx, offset, length int64, onRead func(int64)) (string, error) {
	form := make(map[string]string, len(common)+2)
	for k, v := range common {
		form[k] = v
	}
	form["partIndex"] = strconv.FormatInt(idx, 10)
	form["partSize"] = strconv.FormatInt(length, 10)

	partCtx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	section := io.NewSectionReader(f, offset, length)
	var body io.Reader = driver.NewLimitedUploadStream(partCtx, section)
	if onRead != nil {
		body = &countingReader{r: body, onRead: onRead}
	}

	var resp uploadResp
	req := d.uploadClient.R().
		SetHeaders(d.appHeaders(token)).
		SetHeader("User-Agent", okgoUA).
		SetMultipartFormData(form).
		SetMultipartField("file", name, "application/octet-stream", body).
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
