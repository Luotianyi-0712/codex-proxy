/**
 * 请求转换模块
 * 将 OpenAI Chat Completions 格式的请求转换为 Codex (OpenAI Responses API) 格式
 * 处理消息、工具调用、多模态内容、结构化输出等的格式映射
 */
package translator

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/**
 * ConvertOpenAIRequestToCodex 将 OpenAI Chat Completions 请求转为 Codex Responses API 格式
 *
 * 转换规则：
 *   - messages → input 数组（message/function_call/function_call_output）
 *   - system role → developer role
 *   - tools 中 function 类型展平为顶层 function 定义
 *   - response_format → text.format
 *   - 工具名超过 64 字符自动缩短
 *
 * @param modelName - 模型名称
 * @param rawJSON - 原始 OpenAI Chat Completions 请求 JSON
 * @param stream - 是否为流式请求
 * @returns []byte - 转换后的 Codex Responses API 请求 JSON
 */
func ConvertOpenAIRequestToCodex(modelName string, rawJSON []byte, stream bool) []byte {
	out := `{"instructions":""}`

	out, _ = sjson.Set(out, "stream", stream)

	/*
	 * 映射 reasoning 参数（修复降智问题）
	 * Chat Completions 格式: "reasoning_effort": "high"
	 * Responses API 格式:   "reasoning": {"effort": "high"}
	 * 优先使用请求体中已有的值，不再强制设为 medium
	 */
	if v := gjson.GetBytes(rawJSON, "reasoning_effort"); v.Exists() {
		/* Chat Completions 格式 → 转为 Codex 嵌套格式 */
		out, _ = sjson.Set(out, "reasoning.effort", v.Value())
	} else if v := gjson.GetBytes(rawJSON, "reasoning.effort"); v.Exists() {
		/* Responses API 格式 → 直接透传 */
		out, _ = sjson.Set(out, "reasoning.effort", v.Value())
	}
	/* 注意：不再强制设 medium，让用户/上游自行决定 */
	out, _ = sjson.Set(out, "parallel_tool_calls", true)
	out, _ = sjson.Set(out, "reasoning.summary", "auto")
	out, _ = sjson.Set(out, "include", []string{"reasoning.encrypted_content"})
	out, _ = sjson.Set(out, "model", modelName)

	/* 构建工具名缩短映射 */
	originalToolNameMap := buildToolNameMap(rawJSON)

	/*
	 * 构建 input 数组
	 * Responses API 格式已有 input 字段，直接透传
	 * Chat Completions 格式只有 messages 字段，需要转换为 input
	 */
	existingInput := gjson.GetBytes(rawJSON, "input")
	if existingInput.Exists() && existingInput.IsArray() {
		/*
		 * Responses API 快速路径：直接在原始 JSON 上原地修改
		 * 不重建 JSON，大幅减少序列化开销和内存分配
		 */
		result := make([]byte, len(rawJSON))
		copy(result, rawJSON)
		result, _ = sjson.SetBytes(result, "model", modelName)
		result, _ = sjson.SetBytes(result, "stream", stream)
		result, _ = sjson.SetBytes(result, "store", false)
		if v := gjson.GetBytes(rawJSON, "reasoning_effort"); v.Exists() {
			result, _ = sjson.SetBytes(result, "reasoning.effort", v.Value())
			result, _ = sjson.DeleteBytes(result, "reasoning_effort")
		}

		/* 确保 instructions 存在 */
		if !gjson.GetBytes(result, "instructions").Exists() {
			result, _ = sjson.SetBytes(result, "instructions", "")
		}

		/* 删除上游不支持的参数 */
		result, _ = sjson.DeleteBytes(result, "previous_response_id")
		result, _ = sjson.DeleteBytes(result, "prompt_cache_retention")
		result, _ = sjson.DeleteBytes(result, "safety_identifier")
		result, _ = sjson.DeleteBytes(result, "generate")
		result, _ = sjson.DeleteBytes(result, "max_output_tokens")
		result, _ = sjson.DeleteBytes(result, "service_tier")

		return result
	}

	/* Chat Completions 格式：messages → input 转换 */
	messages := gjson.GetBytes(rawJSON, "messages")
	out, _ = sjson.SetRaw(out, "input", `[]`)

	if messages.IsArray() {
		arr := messages.Array()
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			role := m.Get("role").String()

			switch role {
			case "tool":
				/* tool 消息转为 function_call_output */
				funcOutput := `{}`
				funcOutput, _ = sjson.Set(funcOutput, "type", "function_call_output")
				funcOutput, _ = sjson.Set(funcOutput, "call_id", m.Get("tool_call_id").String())
				funcOutput, _ = sjson.Set(funcOutput, "output", m.Get("content").String())
				out, _ = sjson.SetRaw(out, "input.-1", funcOutput)

			default:
				/* 常规消息 */
				msg := `{}`
				msg, _ = sjson.Set(msg, "type", "message")
				if role == "system" {
					msg, _ = sjson.Set(msg, "role", "developer")
				} else {
					msg, _ = sjson.Set(msg, "role", role)
				}
				msg, _ = sjson.SetRaw(msg, "content", `[]`)

				/* 处理内容 */
				c := m.Get("content")
				if c.Exists() && c.Type == gjson.String && c.String() != "" {
					partType := "input_text"
					if role == "assistant" {
						partType = "output_text"
					}
					part := `{}`
					part, _ = sjson.Set(part, "type", partType)
					part, _ = sjson.Set(part, "text", c.String())
					msg, _ = sjson.SetRaw(msg, "content.-1", part)
				} else if c.Exists() && c.IsArray() {
					items := c.Array()
					for j := 0; j < len(items); j++ {
						it := items[j]
						t := it.Get("type").String()
						switch t {
						case "text":
							partType := "input_text"
							if role == "assistant" {
								partType = "output_text"
							}
							part := `{}`
							part, _ = sjson.Set(part, "type", partType)
							part, _ = sjson.Set(part, "text", it.Get("text").String())
							msg, _ = sjson.SetRaw(msg, "content.-1", part)
						case "image_url":
							if role == "user" {
								part := `{}`
								part, _ = sjson.Set(part, "type", "input_image")
								if u := it.Get("image_url.url"); u.Exists() {
									part, _ = sjson.Set(part, "image_url", u.String())
								}
								msg, _ = sjson.SetRaw(msg, "content.-1", part)
							}
						case "file":
							if role == "user" {
								fileData := it.Get("file.file_data").String()
								filename := it.Get("file.filename").String()
								if fileData != "" {
									part := `{}`
									part, _ = sjson.Set(part, "type", "input_file")
									part, _ = sjson.Set(part, "file_data", fileData)
									if filename != "" {
										part, _ = sjson.Set(part, "filename", filename)
									}
									msg, _ = sjson.SetRaw(msg, "content.-1", part)
								}
							}
						}
					}
				}
				out, _ = sjson.SetRaw(out, "input.-1", msg)

				/* assistant 消息的 tool_calls 转为独立的 function_call 对象 */
				if role == "assistant" {
					toolCalls := m.Get("tool_calls")
					if toolCalls.Exists() && toolCalls.IsArray() {
						tcArr := toolCalls.Array()
						for j := 0; j < len(tcArr); j++ {
							tc := tcArr[j]
							if tc.Get("type").String() == "function" {
								funcCall := `{}`
								funcCall, _ = sjson.Set(funcCall, "type", "function_call")
								funcCall, _ = sjson.Set(funcCall, "call_id", tc.Get("id").String())
								name := tc.Get("function.name").String()
								if short, ok := originalToolNameMap[name]; ok {
									name = short
								} else {
									name = shortenNameIfNeeded(name)
								}
								funcCall, _ = sjson.Set(funcCall, "name", name)
								funcCall, _ = sjson.Set(funcCall, "arguments", tc.Get("function.arguments").String())
								out, _ = sjson.SetRaw(out, "input.-1", funcCall)
							}
						}
					}
				}
			}
		}
	}

	/* 映射 response_format 到 text.format */
	rf := gjson.GetBytes(rawJSON, "response_format")
	if rf.Exists() {
		if !gjson.Get(out, "text").Exists() {
			out, _ = sjson.SetRaw(out, "text", `{}`)
		}
		rft := rf.Get("type").String()
		switch rft {
		case "text":
			out, _ = sjson.Set(out, "text.format.type", "text")
		case "json_schema":
			js := rf.Get("json_schema")
			if js.Exists() {
				out, _ = sjson.Set(out, "text.format.type", "json_schema")
				if v := js.Get("name"); v.Exists() {
					out, _ = sjson.Set(out, "text.format.name", v.Value())
				}
				if v := js.Get("strict"); v.Exists() {
					out, _ = sjson.Set(out, "text.format.strict", v.Value())
				}
				if v := js.Get("schema"); v.Exists() {
					out, _ = sjson.SetRaw(out, "text.format.schema", v.Raw)
				}
			}
		}
	}

	/* 映射 tools */
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		out, _ = sjson.SetRaw(out, "tools", `[]`)
		arr := tools.Array()
		for i := 0; i < len(arr); i++ {
			t := arr[i]
			toolType := t.Get("type").String()

			/*
			 * #1671: 处理 custom 类型工具（如 Codex CLI 的 apply_patch）
			 * Codex CLI 发送 "type": "custom" 的工具定义，包含 Lark 语法格式
			 * 需要将其转换为 Codex Responses API 兼容的格式
			 */
			if toolType == "custom" {
				item := `{}`
				item, _ = sjson.Set(item, "type", "function")
				if v := t.Get("name"); v.Exists() {
					name := v.String()
					if short, ok := originalToolNameMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
					item, _ = sjson.Set(item, "name", name)
				}
				if v := t.Get("description"); v.Exists() {
					item, _ = sjson.Set(item, "description", v.Value())
				}
				/* 将 format 信息编码到 parameters 的 description 中 */
				if format := t.Get("format"); format.Exists() {
					paramSchema := `{"type":"object","properties":{"patch":{"type":"string","description":"The patch content"}}}`
					item, _ = sjson.SetRaw(item, "parameters", paramSchema)
				} else {
					item, _ = sjson.SetRaw(item, "parameters", `{"type":"object","properties":{}}`)
				}
				out, _ = sjson.SetRaw(out, "tools.-1", item)
				continue
			}

			/* 非 function/custom 类型直接透传 */
			if toolType != "" && toolType != "function" && t.IsObject() {
				out, _ = sjson.SetRaw(out, "tools.-1", t.Raw)
				continue
			}

			if toolType == "function" {
				item := `{}`
				item, _ = sjson.Set(item, "type", "function")
				fn := t.Get("function")
				if fn.Exists() {
					if v := fn.Get("name"); v.Exists() {
						name := v.String()
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						item, _ = sjson.Set(item, "name", name)
					}
					if v := fn.Get("description"); v.Exists() {
						item, _ = sjson.Set(item, "description", v.Value())
					}
					if v := fn.Get("parameters"); v.Exists() {
						item, _ = sjson.SetRaw(item, "parameters", v.Raw)
					}
					if v := fn.Get("strict"); v.Exists() {
						item, _ = sjson.Set(item, "strict", v.Value())
					}
				}
				out, _ = sjson.SetRaw(out, "tools.-1", item)
			}
		}
	}

	/* 映射 tool_choice */
	if tc := gjson.GetBytes(rawJSON, "tool_choice"); tc.Exists() {
		switch {
		case tc.Type == gjson.String:
			out, _ = sjson.Set(out, "tool_choice", tc.String())
		case tc.IsObject():
			tcType := tc.Get("type").String()
			if tcType == "function" {
				name := tc.Get("function.name").String()
				if name != "" {
					if short, ok := originalToolNameMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
				}
				choice := `{}`
				choice, _ = sjson.Set(choice, "type", "function")
				if name != "" {
					choice, _ = sjson.Set(choice, "name", name)
				}
				out, _ = sjson.SetRaw(out, "tool_choice", choice)
			} else if tcType != "" {
				out, _ = sjson.SetRaw(out, "tool_choice", tc.Raw)
			}
		}
	}

	/* service_tier 不被 Codex API 支持，不再透传 */

	out, _ = sjson.Set(out, "store", false)
	return []byte(out)
}

/**
 * buildToolNameMap 构建工具名缩短映射表
 * @param rawJSON - 原始请求 JSON
 * @returns map[string]string - 原始名 → 缩短名的映射
 */
func buildToolNameMap(rawJSON []byte) map[string]string {
	m := map[string]string{}
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return m
	}

	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		t := arr[i]
		if t.Get("type").String() == "function" {
			fn := t.Get("function")
			if fn.Exists() {
				if v := fn.Get("name"); v.Exists() {
					names = append(names, v.String())
				}
			}
		}
	}

	if len(names) > 0 {
		return buildShortNameMap(names)
	}
	return m
}

/**
 * shortenNameIfNeeded 对单个工具名进行缩短处理
 * 如果名称超过 64 字符，尝试保留 mcp__ 前缀和最后一段
 * @param name - 工具名
 * @returns string - 缩短后的工具名
 */
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) > limit {
				return candidate[:limit]
			}
			return candidate
		}
	}
	return name[:limit]
}

/**
 * buildShortNameMap 构建唯一的短工具名映射
 * 保留 mcp__ 前缀，使用 _1/_2 后缀确保唯一性
 * @param names - 原始工具名列表
 * @returns map[string]string - 原始名 → 唯一短名的映射
 */
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

/**
 * BuildReverseToolNameMap 从原始 OpenAI 请求构建反向工具名映射
 * 用于将 Codex 响应中缩短的工具名还原为原始名
 * @param originalJSON - 原始 OpenAI 请求 JSON
 * @returns map[string]string - 缩短名 → 原始名的映射
 */
func BuildReverseToolNameMap(originalJSON []byte) map[string]string {
	tools := gjson.GetBytes(originalJSON, "tools")
	rev := map[string]string{}
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return rev
	}

	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		t := arr[i]
		if t.Get("type").String() != "function" {
			continue
		}
		fn := t.Get("function")
		if !fn.Exists() {
			continue
		}
		if v := fn.Get("name"); v.Exists() {
			names = append(names, v.String())
		}
	}

	if len(names) > 0 {
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}
