package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 默认命令白名单（未通过 config 覆盖时使用）。
const defaultAllowedCommands = "ls,cat,echo,pwd,uname,whoami,date,df,du,ps,which,head,tail,wc,env"

// 默认最大输出字节数，防止超长输出导致宿主内存问题。
const maxOutputBytes = 1024 * 1024

// ---- 白名单（可通过插件 config 覆盖）----

var (
	// allowedCommands 是允许执行的命令白名单，按 basename 匹配。
	allowedCommands []string
	// writeDir 是允许写入的目录前缀。
	writeDir string
)

// pluginConfig 是从宿主配置传入的插件配置结构。
// 宿主把 plugins.configs.<pluginID> 下的配置序列化为 JSON 传入 plugin.reconfigure。
type pluginConfig struct {
	AllowedCommands string `json:"allowed_commands"`
	WriteDir        string `json:"write_dir"`
}

// init 用默认值初始化白名单。
func init() {
	allowedCommands = splitTrim(defaultAllowedCommands)
}

// applyConfig 解析配置 JSON 并更新白名单。配置缺失的字段保留默认值。
func applyConfig(rawJSON []byte) {
	var cfg pluginConfig
	if len(rawJSON) > 0 {
		_ = json.Unmarshal(rawJSON, &cfg)
	}
	if strings.TrimSpace(cfg.AllowedCommands) != "" {
		allowedCommands = splitTrim(cfg.AllowedCommands)
	}
	writeDir = strings.TrimSpace(cfg.WriteDir)
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
		return "", fmt.Sprintf("error: command %q is not in the allow-list. Allowed: %s\n", bin, strings.Join(allowedCommands, ", ")), 1
	}

	// 执行命令，分别收集 stdout / stderr。
	cmd := exec.Command(args[0], args[1:]...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		exitCode := 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		return truncate(stdout.String()), truncate(stderr.String()), exitCode
	}
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

	// 目录前缀校验。
	if writeDir != "" {
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
		return "", "error: open file: " + err.Error(), 1
	}
	defer f.Close()

	n, err := f.WriteString(content)
	if err != nil {
		return "", "error: write: " + err.Error(), 1
	}

	return fmt.Sprintf("OK: appended %d bytes to %s\n", n, filePath), "", 0
}

// isAllowedCommand 判断命令 basename 是否在白名单内。
func isAllowedCommand(bin string) bool {
	for _, a := range allowedCommands {
		if a == bin {
			return true
		}
	}
	return false
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
