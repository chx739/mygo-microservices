// 本文件实现 JWT 签发 / 验证核心逻辑。
//
// 关键设计决策：
//  1. 签名算法固定 HS256 —— 单团队对称密钥就够用，免去密钥分发开销。
//  2. Parse 阶段**强制**只接受 HS256，防御「alg=none」与
//     「把公钥当 HMAC 密钥」两类经典算法混淆攻击。
//  3. 每次签发生成新 JTI（UUID），为 Redis 黑名单 / refresh 白名单提供唯一 key。
//  4. 允许 30s 的时间漂移（WithLeeway），避免容器间时钟偏差导致合法 token 被拒。
//  5. Signer 持有密钥副本（copy），调用方修改原 slice 不会影响已构造的 Signer。
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// clockSkew 给 exp / nbf 校验 30s 余量。
// 生产环境容器间可能存在百毫秒级时钟偏移；如果正好卡在边界，
// 不加 leeway 会偶发「刚签发的 token 立刻就过期」这类玄学错误。
const clockSkew = 30 * time.Second

// Signer 封装签发 / 验证所需的所有状态。
//
// 字段说明：
//   - secret：HMAC 密钥原始字节。必须 >= 32 字节（由调用方校验，见 main.go 的 fail-fast）。
//   - issuer：写入 iss 字段；Parse 阶段会强制比对，跨服务复用密钥时能区分。
//   - accessTTL / refreshTTL：两种 token 的有效期。分开设置是为了支持
//     「access 短 + refresh 长」的双 token 策略。
type Signer struct {
	secret     []byte
	issuer     string
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewSigner 构造 Signer。
//
// 参数约束：
//   - secret：至少 32 字节；长度校验在上层（main.go）做，本函数只做 copy 防御。
//   - issuer：非空；若为空，所有 token 都会丢失来源信息，Parse 阶段也会拒绝。
//   - accessTTL / refreshTTL：都必须 > 0；零值会导致签出来的 token 秒级过期。
//
// 返回的 *Signer 是线程安全的 —— 所有字段只读，签发 / 验证方法都不修改内部状态。
func NewSigner(secret []byte, issuer string, accessTTL, refreshTTL time.Duration) *Signer {
	// 复制 secret，避免调用方后续修改 slice 影响已构造的 Signer。
	buf := make([]byte, len(secret))
	copy(buf, secret)
	return &Signer{
		secret:     buf,
		issuer:     issuer,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}
}

// AccessTTL / RefreshTTL 暴露给上层使用，用于：
//   - 登录 / 刷新返回体里 expires_in 字段（避免硬编码 900）。
//   - 登出时计算 access token 剩余有效期，作为 Redis 黑名单 TTL。
func (s *Signer) AccessTTL() time.Duration  { return s.accessTTL }
func (s *Signer) RefreshTTL() time.Duration { return s.refreshTTL }

// IssueAccessToken 签发一个 access token。
//
// 返回 (tokenString, jti, error)：
//   - tokenString：标准紧凑格式 JWT，Bearer 头里直接用。
//   - jti：本 token 的唯一 ID；调用方用它做黑名单 key 或日志关联。
//   - error：HMAC 签名失败时返回，正常情况几乎不会出。
func (s *Signer) IssueAccessToken(userID string, role Role) (string, string, error) {
	return s.issue(userID, role, TokenTypeAccess, s.accessTTL)
}

// IssueRefreshToken 签发一个 refresh token。
//
// 与 access 的区别仅有：TokenType 字段值不同、TTL 更长。
// 上层需要把本函数返回的 jti 写入 Redis `auth:refresh:<jti>`，
// 以实现「撤销 refresh token」能力。
func (s *Signer) IssueRefreshToken(userID string, role Role) (string, string, error) {
	return s.issue(userID, role, TokenTypeRefresh, s.refreshTTL)
}

// issue 是 IssueAccessToken / IssueRefreshToken 的共用实现。
// 抽出来避免两份几乎一样的代码。
func (s *Signer) issue(userID string, role Role, tokenType string, ttl time.Duration) (string, string, error) {
	if userID == "" {
		// 签发空 userID 的 token 没有业务意义，且后续 Parse 回来也无法定位用户。
		return "", "", errors.New("userID is required for token issuance")
	}

	now := time.Now()
	jti := uuid.NewString()

	claims := Claims{
		UserID:    userID,
		Role:      role,
		TokenType: tokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.issuer,
			Subject:   userID,
			ID:        jti, // 填入 jti，Parse 阶段通过 RegisteredClaims.ID 取出。
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", "", fmt.Errorf("sign token: %w", err)
	}
	return signed, jti, nil
}

// Parse 校验并解析一个 token。
//
// 校验内容：
//   - 签名算法必须是 HS256（防算法混淆攻击：alg=none / RSA 公钥当 HMAC key）。
//   - 签名正确（HMAC 对 secret 验证通过）。
//   - iss 字段等于 Signer.issuer。
//   - exp / nbf 在当前时间 ±30s 内有效（WithLeeway 容忍时钟漂移）。
//
// 注意：TokenType 的语义校验（access vs refresh）不在本函数，
// 留给调用方根据使用场景判断（业务接口 / 刷新接口）。
func (s *Signer) Parse(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(
		tokenStr,
		claims,
		func(t *jwt.Token) (any, error) {
			// 强制只接受 HMAC 家族签名；Method 指针比较即可覆盖 HS256。
			// 这是防御算法混淆攻击的关键防线：
			// 如果攻击者把 header 改成 alg=none 或 alg=RS256，
			// 这里会直接拒绝，不会用错误的算法去「验证」。
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return s.secret, nil
		},
		jwt.WithIssuer(s.issuer),
		jwt.WithLeeway(clockSkew),
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		return nil, fmt.Errorf("parse jwt: %w", err)
	}
	return claims, nil
}
