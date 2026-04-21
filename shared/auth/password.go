// 本文件封装 bcrypt 密码哈希。
//
// 为什么不直接在业务代码里调 bcrypt：
//  1. 统一 cost 参数（cost=10）—— 分散配置会造成不同路径写入的 hash 强度不一致。
//  2. 对「空字符串」做显式拒绝 —— bcrypt 自身允许对空字符串生成 hash，
//     这会让「注册时密码为空」被悄悄放过，出现不可登录的死账号。
//  3. 错误语义标准化 —— VerifyPassword 用 bcrypt 的 ErrMismatchedHashAndPassword 包装，
//     handler 层只需判断「nil / 非 nil」即可决定是否返回 401。
package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// passwordCost 控制 bcrypt 工作因子。
//
// 10 是 bcrypt 官方默认值，在 2025 年的服务器上单次哈希 ~60ms。
// 更高的 cost 会显著拖慢登录 / 注册，且对抗字典攻击的边际收益递减。
// 如果未来硬件升级或被压测暴露瓶颈，再调高即可；老 hash 仍可兼容验证。
const passwordCost = 10

// ErrEmptyPassword 表示调用方传入了空密码。
// handler 层收到该错误时应返回 400，而不是 500。
var ErrEmptyPassword = errors.New("password must not be empty")

// HashPassword 对明文密码生成 bcrypt hash。
//
// 为什么主动拒绝空字符串：
//   - bcrypt.GenerateFromPassword("") 不会报错，会产出一个合法的 hash。
//   - 一旦这种 hash 被存进 DB，用户可以用任意空请求重放登录，相当于无密码账号。
//   - 上层注册 handler 虽然也应校验长度，但这里再加一层是零成本的纵深防御。
func HashPassword(plain string) (string, error) {
	if plain == "" {
		return "", ErrEmptyPassword
	}
	b, err := bcrypt.GenerateFromPassword([]byte(plain), passwordCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt generate: %w", err)
	}
	return string(b), nil
}

// VerifyPassword 校验明文密码是否与 hash 匹配。
//
// 返回：
//   - nil：密码正确。
//   - ErrEmptyPassword：调用方传入了空明文（直接拒绝，不走 bcrypt）。
//   - 其他非 nil：密码错误 / hash 格式坏了 —— handler 层一律按 401 处理，
//     不向客户端暴露具体原因，防止用户枚举。
func VerifyPassword(hashed, plain string) error {
	if plain == "" {
		return ErrEmptyPassword
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plain)); err != nil {
		return fmt.Errorf("bcrypt compare: %w", err)
	}
	return nil
}
