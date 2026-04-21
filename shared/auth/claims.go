// Package auth 提供 api-gateway 的 JWT 签发 / 验证与密码哈希能力。
//
// 本文件定义 JWT 自定义 Claims 以及业务需要的枚举常量：
//   - Role：业务侧区分 rider / driver 两种角色；不做细粒度权限树。
//   - TokenType：区分「访问令牌（access）」与「刷新令牌（refresh）」，
//     防止 refresh token 被当作 access token 拿去访问业务接口。
package auth

import "github.com/golang-jwt/jwt/v5"

// Role 是业务角色枚举。
// 刻意用 string 作为底层类型，方便直接写入 JWT payload 与 MongoDB 文档。
type Role string

const (
	RoleRider  Role = "rider"
	RoleDriver Role = "driver"
)

// IsValid 判断角色取值是否在允许集合内。
// 注册接口校验用；避免把任意字符串写进持久化层。
func (r Role) IsValid() bool {
	switch r {
	case RoleRider, RoleDriver:
		return true
	}
	return false
}

// TokenType 取值。用字符串而不是 bool / enum 是为了将来扩展性（比如 "api_key"）。
const (
	TokenTypeAccess  = "access"
	TokenTypeRefresh = "refresh"
)

// Claims 是本服务签发的 JWT 自定义负载。
//
// 字段选择理由：
//   - UserID（uid）：业务主键（内部 UUID），中间件从这里拿用户身份。
//     使用短 key "uid" 是为了让 token 体积小一点。
//   - Role（role）：业务层在部分接口会基于角色做简单分支（比如 /driver/* 只允许 driver），
//     把角色塞进 token 可以免去每次请求都查一次 users 表。
//   - TokenType（typ）：同一个 Signer 会同时签发 access / refresh，中间件必须能区分二者，
//     拒绝用 refresh token 访问业务接口。
//   - RegisteredClaims：标准字段（iss/sub/exp/nbf/iat/jti）。
//     其中 jti 由 uuid.NewString() 填充，用于 Redis 黑名单 & refresh 白名单。
type Claims struct {
	UserID    string `json:"uid"`
	Role      Role   `json:"role"`
	TokenType string `json:"typ"`
	jwt.RegisteredClaims
}
