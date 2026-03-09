/**
 * Claude Messages API 兼容处理器
 * 提供 /v1/messages 端点，接收 Claude 格式请求，转换为 OpenAI 格式后通过 Codex 执行器转发
 * 支持流式和非流式响应，响应结果转换回 Claude Messages API 格式
 * 重试逻辑在 executor 内部完成，客户端不感知账号切换
 */
package handler

import (
	"bufio"
	"fmt"
	"io"
	"net/http"

	"codex-proxy/internal/executor"
	"codex-proxy/internal/translator"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

/**
 * handleMessages 处理 Claude Messages API 请求（/v1/messages）
 * 将 Claude 格式请求转换为 OpenAI 格式 → executor 内部选择账号/重试 → 响应转回 Claude 格式
 */
func (h *ProxyHandler) handleMessages(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		sendClaudeError(c, http.StatusBadRequest, "invalid_request_error", "读取请求体失败")
		return
	}

	openaiBody, model, stream := translator.ConvertClaudeRequestToOpenAI(body)
	if model == "" {
		sendClaudeError(c, http.StatusBadRequest, "invalid_request_error", "缺少 model 字段")
		return
	}

	log.Infof("收到 Claude Messages 请求: model=%s, stream=%v", model, stream)

	rc := h.buildRetryConfig()

	if stream {
		if execErr := h.executeClaudeStream(c, rc, openaiBody, model); execErr != nil {
			handleClaudeExecutorError(c, execErr)
		}
	} else {
		if execErr := h.executeClaudeNonStream(c, rc, openaiBody, model); execErr != nil {
			handleClaudeExecutorError(c, execErr)
		}
	}
}

/**
 * executeClaudeStream 执行 Claude 流式请求
 * 通过 ExecuteRawCodexStream 获取原始 Codex SSE 流（内部已完成重试）
 * 逐行转换为 Claude SSE 事件写回客户端
 *
 * @param c - Gin 上下文
 * @param rc - 内部重试配置
 * @param openaiBody - 已转换为 OpenAI 格式的请求体
 * @param model - 模型名称
 * @returns error - 执行失败时返回错误
 */
func (h *ProxyHandler) executeClaudeStream(c *gin.Context, rc executor.RetryConfig, openaiBody []byte, model string) error {
	rawResp, account, err := h.executor.ExecuteRawCodexStream(c.Request.Context(), rc, openaiBody, model)
	if err != nil {
		return err
	}
	defer func() {
		if rawResp.Body != nil {
			_ = rawResp.Body.Close()
		}
	}()

	/* 只有到这里才开始写 SSE 头（重试在 executor 内部已完成） */
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.WriteHeader(http.StatusOK)

	flusher, canFlush := c.Writer.(http.Flusher)
	state := translator.NewClaudeStreamState(model)

	scanner := bufio.NewScanner(rawResp.Body)
	scanner.Buffer(make([]byte, 4*1024), 50*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		events := translator.ConvertCodexStreamToClaudeEvents(c.Request.Context(), line, state)
		for _, event := range events {
			_, _ = io.WriteString(c.Writer, event)
			if canFlush {
				flusher.Flush()
			}
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		log.Errorf("Claude 读取流式响应失败: %v", scanErr)
		return scanErr
	}

	account.RecordSuccess()
	return nil
}

/**
 * executeClaudeNonStream 执行 Claude 非流式请求
 * 通过 ExecuteRawCodexStream 获取原始 Codex SSE 数据（内部已完成重试）
 * 从中提取结果并转换为 Claude Messages 格式
 *
 * @param c - Gin 上下文
 * @param rc - 内部重试配置
 * @param openaiBody - 已转换为 OpenAI 格式的请求体
 * @param model - 模型名称
 * @returns error - 执行失败时返回错误
 */
func (h *ProxyHandler) executeClaudeNonStream(c *gin.Context, rc executor.RetryConfig, openaiBody []byte, model string) error {
	rawResp, account, err := h.executor.ExecuteRawCodexStream(c.Request.Context(), rc, openaiBody, model)
	if err != nil {
		return err
	}
	defer func() {
		if rawResp.Body != nil {
			_ = rawResp.Body.Close()
		}
	}()

	data, err := io.ReadAll(rawResp.Body)
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	result := translator.ConvertCodexFullSSEToClaudeResponse(c.Request.Context(), data, model)
	if result == "" {
		return fmt.Errorf("未收到 response.completed 事件")
	}

	account.RecordSuccess()
	c.Data(http.StatusOK, "application/json", []byte(result))
	return nil
}

/**
 * handleClaudeExecutorError 处理 Claude 格式的 executor 错误
 * @param c - Gin 上下文
 * @param err - executor 返回的错误
 */
func handleClaudeExecutorError(c *gin.Context, err error) {
	if statusErr, ok := err.(*executor.StatusError); ok {
		sendClaudeError(c, statusErr.Code, "api_error", string(statusErr.Body))
		return
	}
	sendClaudeError(c, http.StatusInternalServerError, "api_error", err.Error())
}

/**
 * sendClaudeError 发送 Claude 格式的错误响应
 * @param c - Gin 上下文
 * @param status - HTTP 状态码
 * @param errType - 错误类型
 * @param message - 错误消息
 */
func sendClaudeError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}
