package emby

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/AmbitiousJun/go-emby2alist/internal/config"
	"github.com/AmbitiousJun/go-emby2alist/internal/service/alist"
	"github.com/AmbitiousJun/go-emby2alist/internal/service/path"
	"github.com/AmbitiousJun/go-emby2alist/internal/util/jsons"
	"github.com/AmbitiousJun/go-emby2alist/internal/util/strs"

	"github.com/gin-gonic/gin"
)

// MediaSourceIdSegment 自定义 MediaSourceId 的分隔符
const MediaSourceIdSegment = "[[_]]"

// getEmbyFileLocalPath 获取 Emby 指定资源的 Path 参数
//
// 优先从缓存空间中获取 PlaybackInfo 数据
//
// uri 中必须有 query 参数 MediaSourceId,
// 如果没有携带该参数, 可能会请求到多个资源, 默认返回第一个资源
func getEmbyFileLocalPath(itemInfo ItemInfo) (string, error) {
	var body *jsons.Item

	if cacheBody, ok := getPlaybackInfoByCacheSpace(itemInfo); ok {
		body = cacheBody
	} else {
		res, _ := Fetch(itemInfo.PlaybackInfoUri, http.MethodPost, nil)
		if res.Code != http.StatusOK {
			return "", fmt.Errorf("请求 Emby 接口异常, error: %s", res.Msg)
		}
		body = res.Data
	}

	mediaSources, ok := body.Attr("MediaSources").Done()
	if !ok {
		return "", fmt.Errorf("获取不到 MediaSources, 原始响应: %v", body)
	}

	var path string
	var defaultPath string

	reqId, _ := url.QueryUnescape(itemInfo.MsInfo.RawId)
	// 获取指定 MediaSourceId 的 Path
	mediaSources.RangeArr(func(_ int, value *jsons.Item) error {
		if strs.AnyEmpty(defaultPath) {
			// 默认选择第一个路径
			defaultPath, _ = value.Attr("Path").String()
		}
		if itemInfo.MsInfo.Empty {
			// 如果没有传递 MediaSourceId, 就使用默认的 Path
			return jsons.ErrBreakRange
		}

		curId, _ := url.QueryUnescape(value.Attr("Id").Val().(string))
		if curId == reqId {
			path, _ = value.Attr("Path").String()
			return jsons.ErrBreakRange
		}
		return nil
	})

	if strs.AllNotEmpty(path) {
		return path, nil
	}
	if strs.AllNotEmpty(defaultPath) {
		return defaultPath, nil
	}
	return "", fmt.Errorf("获取不到 Path 参数, 原始响应: %v", body)
}

// findVideoPreviewInfos 查找 source 的所有转码资源
//
// 传递 resChan 进行异步查询, 通过监听 resChan 获取查询结果
func findVideoPreviewInfos(source *jsons.Item, originName string, resChan chan []*jsons.Item) {
	if source == nil || source.Type() != jsons.JsonTypeObj {
		resChan <- nil
		return
	}

	// 转换 alist 绝对路径
	alistPathRes := path.Emby2Alist(source.Attr("Path").Val().(string))
	var transcodingList *jsons.Item
	firstFetchSuccess := false
	if alistPathRes.Success {
		res := alist.FetchFsOther(alistPathRes.Path, nil)

		if res.Code == http.StatusOK {
			if list, ok := res.Data.Attr("video_preview_play_info").Attr("live_transcoding_task_list").Done(); ok {
				firstFetchSuccess = true
				transcodingList = list
			}
		}

		if res.Code == http.StatusForbidden {
			resChan <- nil
			return
		}
	}

	// 首次请求失败, 遍历 alist 所有根目录, 重新请求
	if !firstFetchSuccess {
		paths, err := alistPathRes.Range()
		if err != nil {
			log.Printf("转换 alist 路径异常: %v", err)
			resChan <- nil
			return
		}

		for i := 0; i < len(paths); i++ {
			res := alist.FetchFsOther(paths[i], nil)
			if res.Code == http.StatusOK {
				if list, ok := res.Data.Attr("video_preview_play_info").Attr("live_transcoding_task_list").Done(); ok {
					transcodingList = list
				}
				break
			}
		}
	}

	if transcodingList == nil ||
		transcodingList.Empty() ||
		transcodingList.Type() != jsons.JsonTypeArr {
		resChan <- nil
		return
	}

	res := make([]*jsons.Item, transcodingList.Len())
	wg := sync.WaitGroup{}
	transcodingList.RangeArr(func(idx int, transcode *jsons.Item) error {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copySource := jsons.NewByVal(source.Struct())
			templateId, _ := transcode.Attr("template_id").String()
			templateWidth, _ := transcode.Attr("template_width").Int()
			templateHeight, _ := transcode.Attr("template_height").Int()
			playlistUrl, _ := transcode.Attr("url").String()
			format := fmt.Sprintf("%dx%d", templateWidth, templateHeight)
			copySource.Attr("Name").Set(fmt.Sprintf("(%s_%s) %s", templateId, format, originName))

			// 重要！！！这里的 id 必须和原本的 id 不一样, 但又要确保能够正常反推出原本的 id
			newId := fmt.Sprintf(
				"%s%s%s%s%s%s%s",
				source.Attr("Id").Val(), MediaSourceIdSegment,
				templateId, MediaSourceIdSegment,
				format, MediaSourceIdSegment,
				url.QueryEscape(alistPathRes.Path),
			)
			copySource.Attr("Id").Set(newId)

			// 设置转码代理播放链接
			tu, _ := url.Parse("/videos/proxy_playlist")
			q := tu.Query()
			q.Set("alist_path", alistPathRes.Path)
			q.Set("template_id", templateId)
			q.Set(QueryApiKeyName, config.C.Emby.ApiKey)
			q.Set("remote", playlistUrl)
			tu.RawQuery = q.Encode()

			// 标记转码资源使用转码容器
			copySource.Put("SupportsTranscoding", jsons.NewByVal(true))
			copySource.Put("TranscodingContainer", jsons.NewByVal("ts"))
			copySource.Put("TranscodingSubProtocol", jsons.NewByVal("hls"))
			copySource.Put("TranscodingUrl", jsons.NewByVal(tu.String()))
			copySource.DelKey("DirectStreamUrl")
			copySource.Put("SupportsDirectPlay", jsons.NewByVal(false))
			copySource.Put("SupportsDirectStream", jsons.NewByVal(false))

			res[idx] = copySource
		}()
		return nil
	})
	wg.Wait()

	resChan <- res
}

// findMediaSourceName 查找 MediaSource 中的视频名称, 如 '1080p HEVC'
func findMediaSourceName(source *jsons.Item) string {
	if source == nil || source.Type() != jsons.JsonTypeObj {
		return ""
	}

	mediaStreams, ok := source.Attr("MediaStreams").Done()
	if !ok || mediaStreams.Type() != jsons.JsonTypeArr {
		return source.Attr("Name").Val().(string)
	}

	idx := mediaStreams.FindIdx(func(val *jsons.Item) bool {
		return val.Attr("Type").Val() == "Video"
	})
	if idx == -1 {
		return source.Attr("Name").Val().(string)
	}
	return mediaStreams.Ti().Idx(idx).Attr("DisplayTitle").Val().(string)
}

// itemIdRegex 用于匹配出请求 uri 中的 itemId
var itemIdRegex = regexp.MustCompile(`(?:/emby)?/.*/(\d+)(?:/|\?)?`)

// resolveItemInfo 解析 emby 资源 item 信息
func resolveItemInfo(c *gin.Context) (ItemInfo, error) {
	if c == nil {
		return ItemInfo{}, errors.New("参数 c 不能为空")
	}

	uri := c.Request.RequestURI
	matches := itemIdRegex.FindStringSubmatch(uri)
	if len(matches) < 2 {
		return ItemInfo{}, fmt.Errorf("itemId 匹配失败, uri: %s", uri)
	}
	itemInfo := ItemInfo{Id: matches[1], ApiKey: c.Query("QueryTokenName")}

	if itemInfo.ApiKey == "" {
		itemInfo.ApiKey = c.Query(QueryApiKeyName)
	}
	if itemInfo.ApiKey == "" {
		itemInfo.ApiKey = config.C.Emby.ApiKey
	}

	msInfo, err := resolveMediaSourceId(getRequestMediaSourceId(c))
	if err != nil {
		return ItemInfo{}, fmt.Errorf("解析 MediaSource 失败, uri: %s, err: %v", uri, err)
	}
	itemInfo.MsInfo = msInfo

	u, err := url.Parse(fmt.Sprintf("/Items/%s/PlaybackInfo", itemInfo.Id))
	if err != nil {
		return ItemInfo{}, fmt.Errorf("构建 PlaybackInfo uri 失败, err: %v", err)
	}
	q := u.Query()
	q.Set(QueryApiKeyName, itemInfo.ApiKey)
	if !msInfo.Empty {
		q.Set("MediaSourceId", msInfo.OriginId)
	}
	u.RawQuery = q.Encode()
	itemInfo.PlaybackInfoUri = u.String()

	return itemInfo, nil
}

// getRequestMediaSourceId 尝试从请求参数或请求体中获取 MediaSourceId 信息
//
// 优先返回请求参数中的值, 如果两者都获取不到, 就返回空字符串
func getRequestMediaSourceId(c *gin.Context) string {
	if c == nil {
		return ""
	}

	// 1 从请求参数中获取
	q := c.Query("MediaSourceId")
	if strs.AllNotEmpty(q) {
		return q
	}

	// 2 从请求体中获取
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return ""
	}
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	reqJson, err := jsons.New(string(bodyBytes))
	if err != nil {
		return ""
	}
	if msId, ok := reqJson.Attr("MediaSourceId").String(); ok {
		return msId
	}
	return ""
}

// resolveMediaSourceId 解析 MediaSourceId
func resolveMediaSourceId(id string) (MsInfo, error) {
	res := MsInfo{Empty: true, RawId: id}

	if id == "" {
		return res, nil
	}
	res.Empty = false

	if len(id) <= 32 {
		res.OriginId = id
		return res, nil
	}

	segments := strings.Split(id, MediaSourceIdSegment)
	if len(segments) != 4 {
		return MsInfo{}, errors.New("MediaSourceId 格式错误: " + id)
	}

	res.Transcode = true
	res.OriginId = segments[0]
	res.TemplateId = segments[1]
	res.Format = segments[2]
	res.AlistPath = segments[3]
	res.SourceNamePrefix = fmt.Sprintf("%s_%s", res.TemplateId, res.Format)
	return res, nil
}
