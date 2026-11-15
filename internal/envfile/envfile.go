// Package envfile loads simple KEY=value files for local development.
package envfile

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Load 读取配置文件并将变量写入进程环境；同名的系统环境变量优先保留。
// 文件不存在时不报错，调用方可以继续使用默认值或部署环境提供的变量。
func Load(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open env file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// 扩大单行限制，以支持较长的 URL、证书内容或密钥配置。
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		// 空行和注释行不参与配置解析。
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !validKey(key) {
			return fmt.Errorf("env file line %d: expected KEY=value", lineNumber)
		}
		value, err = parseValue(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("env file line %d: %w", lineNumber, err)
		}
		// 不覆盖操作系统或启动脚本显式设置的环境变量。
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("env file line %d: set %s: %w", lineNumber, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read env file: %w", err)
	}
	return nil
}

// String 返回非空环境变量；变量不存在或为空时返回 fallback。
func String(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// validKey 检查变量名是否符合常见环境变量命名规则。
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// parseValue 去掉未加引号值的首尾空格，并解析单引号或双引号包裹的值。
func parseValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value[0] == '"' {
		parsed, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("invalid quoted value: %w", err)
		}
		return parsed, nil
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", errors.New("missing closing quote")
		}
		return value[1 : len(value)-1], nil
	}
	return strings.TrimSpace(value), nil
}
