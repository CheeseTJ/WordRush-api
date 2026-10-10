// Package speak 对接 OpenPronounce 发音评测微服务：
// 前端上传跟读音频，转发给评测服务打分，返回 0-100 分与音素级错误诊断。
//
// 评测服务未配置或不可用时返回 ErrUnavailable，前端自动降级浏览器识别。
package speak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// ErrUnavailable 表示评测服务不可用（未配置 / 不可达 / 5xx）。
var ErrUnavailable = errors.New("发音评测服务不可用")

// ErrBadAudio 表示音频无法解析（空文件 / 格式不支持）。
var ErrBadAudio = errors.New("音频无法解析")

// 评测输入上限：2MB 足够 30 秒 opus/webm 录音。
const maxAudioBytes = 2 << 20

// Client 是 OpenPronounce HTTP 客户端。
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient 创建客户端；baseURL 形如 http://127.0.0.1:8100。
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// WordError 是一个读错的词（IPA 对照与置信度）。
type WordError struct {
	Word       string  `json:"word"`
	Expected   string  `json:"expected"`
	Actual     string  `json:"actual"`
	Confidence float64 `json:"confidence"`
}

// AssessResult 是一次跟读评测结果。
type AssessResult struct {
	Score      int         `json:"score"`     // 0-100
	Transcript string      `json:"transcript"` // 评测服务听到的内容
	OK         bool        `json:"ok"`        // score >= PassScore
	Errors     []WordError `json:"errors,omitempty"`
}

// PassScore 是跟读通过的分数线。
// 单词短音频在音素识别上有偶发误报（OpenPronounce 已知限制），阈值放宽到 50。
const PassScore = 50

// opResult 是 OpenPronounce /pronunciation 响应中我们关心的字段子集。
// 注意：转写文本的字段名实测为 "transcribe"（README 写的 transcription 有误）。
type opResult struct {
	Score        float64 `json:"score"`
	Transcription string `json:"transcribe"`
	Differences  struct {
		Errors []struct {
			Word       string      `json:"word"`
			Expected   string      `json:"expected"`
			Actual     *string     `json:"actual"` // 缺失发音时为 null
			Confidence float64     `json:"confidence"`
		} `json:"errors"`
	} `json:"differences"`
}

// Assess 评测一段跟读音频。word 为目标词（英文），audio 为原始音频字节
// （任意 ffmpeg 可读格式，由评测服务转码）。
func (c *Client) Assess(ctx context.Context, word string, audio []byte, contentType string) (*AssessResult, error) {
	if len(audio) == 0 {
		return nil, ErrBadAudio
	}
	if len(audio) > maxAudioBytes {
		return nil, fmt.Errorf("音频超过 %d 字节上限", maxAudioBytes)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("expected_text", word); err != nil {
		return nil, err
	}
	if err := mw.WriteField("lang", "en"); err != nil {
		return nil, err
	}
	fw, err := mw.CreateFormFile("file", "recording"+extOf(contentType))
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(audio); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/pronunciation", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}

	var op opResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&op); err != nil {
		return nil, fmt.Errorf("评测响应解析失败: %w", err)
	}

	out := &AssessResult{
		Score:      int(op.Score + 0.5),
		Transcript: strings.ToLower(strings.TrimSpace(op.Transcription)),
		OK:         op.Score >= PassScore,
		Errors:     make([]WordError, 0, len(op.Differences.Errors)),
	}
	if out.Score < 0 {
		out.Score = 0
	}
	if out.Score > 100 {
		out.Score = 100
	}
	for _, e := range op.Differences.Errors {
		actual := ""
		if e.Actual != nil {
			actual = *e.Actual
		}
		out.Errors = append(out.Errors, WordError{
			Word:       e.Word,
			Expected:   e.Expected,
			Actual:     actual,
			Confidence: e.Confidence,
		})
	}
	return out, nil
}

// extOf 按上传的 MIME 猜一个扩展名（评测服务靠 ffmpeg 认格式，扩展名只是辅助）。
func extOf(contentType string) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "webm"):
		return ".webm"
	case strings.Contains(ct, "ogg"):
		return ".ogg"
	case strings.Contains(ct, "mp4") || strings.Contains(ct, "aac") || strings.Contains(ct, "m4a"):
		return ".mp4"
	case strings.Contains(ct, "wav"):
		return ".wav"
	case strings.Contains(ct, "mpeg"):
		return ".mp3"
	default:
		return ".webm"
	}
}
