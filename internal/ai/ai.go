// Package ai 对接 Cloudflare Workers AI 的对话接口（OpenAI 兼容端点）。
//
// 口语对话的文本环节：前端把消息列表发来，本包转发给 Workers AI
// （默认 GLM-4.7-Flash）取回文本回复，再由前端用本地 Piper TTS 朗读。
// 未配置凭证或上游不可用时返回 ErrUnavailable，前端据此降级提示。
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnavailable 表示对话服务不可用（未配置 / 不可达 / 上游非 200）。
var ErrUnavailable = errors.New("AI 对话服务不可用")

// ErrQuotaExhausted 表示当日免费额度（Neurons）已用完，需等到次日重置。
// 与 ErrUnavailable 独立：前端据此给出"明天再聊"的友好提示而非通用错误。
var ErrQuotaExhausted = errors.New("今日对话额度已用完")

// cfErrorCodeNeurons 是 Cloudflare 额度耗尽时返回的内部错误码。
const cfErrorCodeNeurons = 4006

// Message 是一条对话消息，role 为 user / assistant / system。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client 是 Workers AI 对话客户端。
type Client struct {
	endpoint       string
	token          string
	model          string
	enableThinking bool
	http           *http.Client
}

// NewClient 创建客户端；endpoint 为 OpenAI 兼容的 chat completions 完整地址。
// enableThinking 控制是否开启推理模型的思考链（闲聊建议关闭以省额度）。
func NewClient(endpoint, token, model string, enableThinking bool, timeout time.Duration) *Client {
	return &Client{
		endpoint:       endpoint,
		token:          token,
		model:          model,
		enableThinking: enableThinking,
		http:           &http.Client{Timeout: timeout},
	}
}

type chatTemplateKwargs struct {
	EnableThinking bool `json:"enable_thinking"`
}

type chatRequest struct {
	Model              string              `json:"model"`
	Messages           []Message           `json:"messages"`
	MaxTokens          int                 `json:"max_tokens,omitempty"`
	ChatTemplateKwargs *chatTemplateKwargs `json:"chat_template_kwargs,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Chat 发送消息列表并返回助手回复文本。maxTokens<=0 时不下发该字段。
func (c *Client) Chat(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	reqBody := chatRequest{
		Model:              c.model,
		Messages:           messages,
		MaxTokens:          maxTokens,
		ChatTemplateKwargs: &chatTemplateKwargs{EnableThinking: c.enableThinking},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		// 额度耗尽：CF 返回 429，错误体 errors[].code = 4006。
		// 单独识别，让前端能给出"明天再聊"而非通用错误提示。
		if resp.StatusCode == http.StatusTooManyRequests || hasCFErrorCode(raw, cfErrorCodeNeurons) {
			return "", fmt.Errorf("%w: %s", ErrQuotaExhausted, snippet(raw))
		}
		return "", fmt.Errorf("%w: HTTP %d %s", ErrUnavailable, resp.StatusCode, snippet(raw))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("对话响应解析失败: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%w: 上游返回空回复", ErrUnavailable)
	}
	reply := strings.TrimSpace(out.Choices[0].Message.Content)
	if reply == "" {
		return "", fmt.Errorf("%w: 上游返回空回复", ErrUnavailable)
	}
	return reply, nil
}

// snippet 截断上游错误体，避免回显过大或含敏感信息。
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// cfErrorBody 是 Cloudflare API 的错误体结构（OpenAI 兼容端点也用它）。
type cfErrorBody struct {
	Errors []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// hasCFErrorCode 判断错误体中是否含指定内部错误码。
func hasCFErrorCode(raw []byte, code int) bool {
	var body cfErrorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return false
	}
	for _, e := range body.Errors {
		if e.Code == code {
			return true
		}
	}
	return false
}
