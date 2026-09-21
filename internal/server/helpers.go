package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"golang.org/x/crypto/bcrypt"
)

// bcryptHash 使用 bcrypt 哈希密码。
func bcryptHash(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// bcryptCompare 校验密码与哈希是否匹配。
func bcryptCompare(pw, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// sha256Hex 计算 SHA-256 十六进制摘要（用于 Agent token 落库）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// jsonUnmarshal / jsonMarshal 统一 JSON 编解码入口。
func jsonUnmarshal(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
