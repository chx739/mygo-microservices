package main

// user_repo.go 是 users collection 的持久化入口。
//
// 设计要点：
//  1. 把 Repo 抽成 interface (UserRepository)，handler 层不依赖具体实现，便于测试替换。
//  2. 业务主键用 UUID（而不是 Mongo ObjectID），理由：UUID 可以不暴露 Mongo 内部 ID，
//     也便于未来换存储层；ObjectID 只是 bson `_id` 的默认值，跨系统不稳定。
//  3. `phone` 唯一 —— 由 main.go 的 EnsureUserIndexes 统一建索引；Create 时检测 duplicate key
//     并转换为语义化的 ErrPhoneTaken，避免把 Mongo 原生错误泄漏到 handler。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// usersCollection 是本仓库绑定的 Mongo 集合名。
// 抽出常量避免多处硬编码，测试里可以通过 NewMongoUserRepoWithCollection 覆盖。
const usersCollection = "users"

// UserDoc 是 users collection 的 BSON 文档结构。
//
// 字段说明：
//   - ID：Mongo 自动分配的 _id，只在内部使用；对外永远用 UUID。
//   - UUID：业务主键；注册时生成，登录返回给客户端，之后在 token 里作为 sub / uid。
//   - Phone：登录凭证，唯一。
//   - PasswordHash：bcrypt hash；严禁在日志 / 返回体里出现。
//   - Role：业务角色；字符串而不是 auth.Role 是因为 bson 序列化对 interface 不友好。
//   - CreatedAt / UpdatedAt：标准时间戳；UpdatedAt 目前未用，保留以备修改密码场景。
type UserDoc struct {
	ID           primitive.ObjectID `bson:"_id,omitempty"`
	UUID         string             `bson:"uuid"`
	Phone        string             `bson:"phone"`
	PasswordHash string             `bson:"password_hash"`
	Role         string             `bson:"role"`
	CreatedAt    time.Time          `bson:"created_at"`
	UpdatedAt    time.Time          `bson:"updated_at"`
}

// Sentinel errors —— handler 层用 errors.Is 精确判断业务错误。
var (
	// ErrUserNotFound：FindByPhone / FindByUUID 未命中。
	// 注意：登录 handler 收到这个错误也必须返回 401 而不是 404，防止手机号枚举。
	ErrUserNotFound = errors.New("user not found")

	// ErrPhoneTaken：Create 时触发了 phone 唯一索引冲突。
	// handler 收到这个错误应返回 409，提示用户换手机号或去登录。
	ErrPhoneTaken = errors.New("phone already registered")
)

// UserRepository 是用户持久化层的抽象。
// handler 只依赖这个接口；真实 Mongo 实现在 mongoUserRepo。
// 目前 interface 只暴露了 Create / FindByPhone / FindByUUID 三个方法，
// 足够注册 + 登录场景；将来修改密码等功能再扩展即可。
type UserRepository interface {
	Create(ctx context.Context, u *UserDoc) error
	FindByPhone(ctx context.Context, phone string) (*UserDoc, error)
	FindByUUID(ctx context.Context, uuid string) (*UserDoc, error)
}

type mongoUserRepo struct {
	col *mongo.Collection
}

// NewMongoUserRepo 绑定默认集合名 "users"。
// 生产代码走这一条；测试里如果需要隔离集合，用 NewMongoUserRepoWithCollection。
func NewMongoUserRepo(db *mongo.Database) UserRepository {
	return NewMongoUserRepoWithCollection(db, usersCollection)
}

// NewMongoUserRepoWithCollection 允许指定集合名，供集成测试生成隔离的临时集合。
// 生产调用栈不会走这里，因此不在 UserRepository 接口里暴露。
func NewMongoUserRepoWithCollection(db *mongo.Database, collection string) *mongoUserRepo {
	return &mongoUserRepo{col: db.Collection(collection)}
}

// Create 写入一条新用户。
//
// 行为：
//   - 若 u.UUID 为空，自动生成一个；避免 handler 忘设导致后续无法定位用户。
//   - CreatedAt / UpdatedAt 如果调用方没设，用当前时间兜底；保持 DB 里不会出现零值时间戳。
//   - 唯一索引冲突（phone）→ 返回 ErrPhoneTaken，调用方按 409 处理。
func (r *mongoUserRepo) Create(ctx context.Context, u *UserDoc) error {
	if u == nil {
		return errors.New("user doc is required")
	}
	if u.Phone == "" {
		return errors.New("phone is required")
	}
	if u.PasswordHash == "" {
		return errors.New("password hash is required")
	}
	if u.UUID == "" {
		u.UUID = uuid.NewString()
	}
	now := time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = now
	}

	if _, err := r.col.InsertOne(ctx, u); err != nil {
		// duplicate key 可能来自 phone 唯一索引；转换为业务错误。
		if mongo.IsDuplicateKeyError(err) {
			return ErrPhoneTaken
		}
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// FindByPhone 根据手机号查询。
// 未命中返回 ErrUserNotFound —— handler 会把它转换成 401（防枚举）。
func (r *mongoUserRepo) FindByPhone(ctx context.Context, phone string) (*UserDoc, error) {
	if phone == "" {
		return nil, errors.New("phone is required")
	}
	var doc UserDoc
	err := r.col.FindOne(ctx, bson.M{"phone": phone}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("find user by phone: %w", err)
	}
	return &doc, nil
}

// FindByUUID 根据业务主键查询。
// 用于 /auth/me 之类需要从 token 反查用户详情的接口（当前没有直接调用，但保留）。
func (r *mongoUserRepo) FindByUUID(ctx context.Context, id string) (*UserDoc, error) {
	if id == "" {
		return nil, errors.New("uuid is required")
	}
	var doc UserDoc
	err := r.col.FindOne(ctx, bson.M{"uuid": id}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("find user by uuid: %w", err)
	}
	return &doc, nil
}

// EnsureUserIndexes 在 main.go 启动阶段创建 users 集合需要的索引。
//
// 当前只建一个：phone 字段唯一索引。
// 选用唯一索引而不是应用层去重的原因：
//   - 多副本并发 Create 时，应用层 "先查后写" 存在 TOCTOU，无法真正保证唯一。
//   - MongoDB 唯一索引在写入端就会返回 duplicate key，精确且无需分布式锁。
//
// 幂等性：Indexes().CreateOne 在索引已存在且定义相同时不会报错，Mongo 保证。
func EnsureUserIndexes(ctx context.Context, database *mongo.Database) error {
	col := database.Collection(usersCollection)
	_, err := col.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "phone", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("uniq_phone"),
	})
	if err != nil {
		return fmt.Errorf("create users.phone unique index: %w", err)
	}
	return nil
}
