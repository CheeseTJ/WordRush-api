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
	"regexp"
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

// PhoneDiff 是一个音素的期望与实际读音（IPA），heard 为空表示漏读。
type PhoneDiff struct {
	Expected string `json:"expected"`
	Heard    string `json:"heard"`
}

// WordError 是一个读错的词（IPA 对照与置信度）。
type WordError struct {
	Word       string      `json:"word"`
	Expected   string      `json:"expected"`
	Actual     string      `json:"actual"`
	Confidence float64     `json:"confidence"`
	Phones     []PhoneDiff `json:"phones,omitempty"` // 逐音素对照，heard 与 expected 不同即读错
}

// AssessResult 是一次跟读评测结果。
type AssessResult struct {
	Score          int         `json:"score"`     // 0-100
	Transcript     string      `json:"transcript"` // 评测服务听到的内容
	OK             bool        `json:"ok"`        // score >= PassScore
	Errors         []WordError `json:"errors,omitempty"`
	ExpectedPhones []string    `json:"expected_phones,omitempty"` // 目标词完整音素序列
	HeardPhones    []string    `json:"heard_phones,omitempty"`    // 实际听到的音素序列（近音小误差时 errors 为空，用它逐位比对）
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
			Phones     []struct {
				Expected string `json:"expected"`
				Heard    string `json:"heard"`
			} `json:"phones"`
		} `json:"errors"`
		ExpectedPhones [][]string `json:"expected_phones"` // 每个词的完整音素序列
		HeardPhones    []string   `json:"heard_phones"`    // 实际听到的音素序列（扁平）
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
		phones := make([]PhoneDiff, 0, len(e.Phones))
		for _, p := range e.Phones {
			phones = append(phones, PhoneDiff{Expected: p.Expected, Heard: p.Heard})
		}
		out.Errors = append(out.Errors, WordError{
			Word:       e.Word,
			Expected:   e.Expected,
			Actual:     actual,
			Confidence: e.Confidence,
			Phones:     phones,
		})
	}
	// 单词跟读场景只有一个目标词，取它的完整音素序列
	if len(op.Differences.ExpectedPhones) > 0 {
		out.ExpectedPhones = op.Differences.ExpectedPhones[0]
	}
	if len(op.Differences.HeardPhones) > 0 {
		out.HeardPhones = op.Differences.HeardPhones
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

/* ==================== 整句转写（口语对话输入） ==================== */

// Transcribe 调 OpenPronounce /speech2text，把整段录音转成文本。
// 与 Assess 不同，这里不提供目标文本，用于口语对话的自由输入。
func (c *Client) Transcribe(ctx context.Context, audio []byte, contentType string) (string, error) {
	if len(audio) == 0 {
		return "", ErrBadAudio
	}
	if len(audio) > maxAudioBytes {
		return "", fmt.Errorf("音频超过 %d 字节上限", maxAudioBytes)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("lang", "en"); err != nil {
		return "", err
	}
	fw, err := mw.CreateFormFile("file", "recording"+extOf(contentType))
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(audio); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/speech2text", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("%w: HTTP %d", ErrUnavailable, resp.StatusCode)
	}

	var out struct {
		Transcript string `json:"transcript"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("转写响应解析失败: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(out.Transcript)), nil
}

/* ==================== 发音 TTS（迭代四补充） ==================== */

// VoicePattern 是合法 Piper voice id 的格式（防任意模型下载）。
var voicePattern = regexp.MustCompile(`^[a-z]{2}_[A-Za-z]{2}-[a-z]+-(x_low|low|medium|high)$`)

// ValidVoice 校验 voice id 是否为合法 Piper 格式。
func ValidVoice(voice string) bool {
	if voice == "" {
		return true // 空 = 服务端默认音色
	}
	return voicePattern.MatchString(voice)
}

// Synthesize 调 OpenPronounce /tts 合成参考发音，返回 16kHz 单声道 WAV。
// text 为待合成文本，voice 为 Piper 音色 id（空 = 服务端默认）。
func (c *Client) Synthesize(ctx context.Context, text, voice string) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("text", text); err != nil {
		return nil, err
	}
	if err := mw.WriteField("lang", "en"); err != nil {
		return nil, err
	}
	if voice != "" {
		if err := mw.WriteField("voice", voice); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/tts", &buf)
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
	wav, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if len(wav) < 44 { // 连 WAV 头都不到，视为失败
		return nil, ErrBadAudio
	}
	return wav, nil
}
