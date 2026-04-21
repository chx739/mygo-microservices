package main

// user_repo_test.go 跑在真实 MongoDB 上。
//
// 跳过约定：
//   环境变量 MONGODB_URI_TEST 未设置 → 整个文件 t.Skip，CI 无 Mongo 时不挂。
//   默认值 mongodb://localhost:27017/test_jwt；调用方（本地 / CI）显式 docker run 后再导出。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	mongoURIEnv    = "MONGODB_URI_TEST"
	defaultMongoDB = "test_jwt"
)

// startMongoForTest 建立一个临时的 Mongo DB + 唯一集合组合，供单个测试使用。
// 返回 (database, colName)：测试用 colName 通过 NewMongoUserRepoWithCollection 绑定。
// cleanup 挂到 t.Cleanup，调用方无需再手动调。
//
// 为什么每个测试独立集合：
//   - 并发测试可以跑，不会互相串数据。
//   - 唯一索引的「重复创建」测试不会污染其他测试。
func startMongoForTest(t *testing.T) (*mongo.Database, string) {
	t.Helper()

	uri := os.Getenv(mongoURIEnv)
	if uri == "" {
		t.Skipf("skip: %s not set; run `docker run -d --rm --name test-mongo -p 27017:27017 mongo:7` and export %s=mongodb://localhost:27017/%s",
			mongoURIEnv, mongoURIEnv, defaultMongoDB)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		t.Fatalf("ping mongo: %v", err)
	}

	dbName := defaultMongoDB
	// 允许用 URI 里的 db 名覆盖默认。
	if parsed, err := parseDBFromURI(uri); err == nil && parsed != "" {
		dbName = parsed
	}
	// 每个测试用独立集合名，避免并发串数据。
	colName := fmt.Sprintf("users_%s_%s",
		strings.ReplaceAll(t.Name(), "/", "_"),
		uuid.NewString()[:8],
	)

	database := client.Database(dbName)
	// 先建索引（等同于 EnsureUserIndexes，但针对自定义集合名）。
	if _, err := database.Collection(colName).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "phone", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("uniq_phone_test"),
	}); err != nil {
		_ = client.Disconnect(context.Background())
		t.Fatalf("create test index: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = database.Collection(colName).Drop(ctx)
		_ = client.Disconnect(ctx)
	})

	return database, colName
}

// parseDBFromURI 从 mongodb://host[:port]/dbname 形式的 URI 里摘出 db 名。
// 如果没有 db 部分，返回 ("", nil)。
func parseDBFromURI(uri string) (string, error) {
	// 极简实现，不处理 srv/auth source 等复杂情况，测试场景够用。
	_, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return "", fmt.Errorf("bad uri: %s", uri)
	}
	_, tail, ok := strings.Cut(rest, "/")
	if !ok {
		return "", nil
	}
	db, _, _ := strings.Cut(tail, "?")
	return db, nil
}

// repoFor 把 startMongoForTest 生成的 database + colName 包成 Repo。
func repoFor(t *testing.T, database *mongo.Database, colName string) *mongoUserRepo {
	t.Helper()
	return NewMongoUserRepoWithCollection(database, colName)
}

func TestMongoUserRepo_Create_Basic(t *testing.T) {
	t.Parallel()

	database, colName := startMongoForTest(t)
	repo := repoFor(t, database, colName)

	doc := &UserDoc{
		Phone:        "13800138000",
		PasswordHash: "hash-xx",
		Role:         "rider",
	}
	if err := repo.Create(context.Background(), doc); err != nil {
		t.Fatalf("create: %v", err)
	}
	if doc.UUID == "" {
		t.Fatalf("UUID should be auto-filled after Create")
	}

	got, err := repo.FindByPhone(context.Background(), "13800138000")
	if err != nil {
		t.Fatalf("find by phone: %v", err)
	}
	if got.UUID != doc.UUID {
		t.Fatalf("UUID mismatch: got %q want %q", got.UUID, doc.UUID)
	}
	if got.PasswordHash != "hash-xx" {
		t.Fatalf("PasswordHash mismatch: got %q", got.PasswordHash)
	}
	if got.Role != "rider" {
		t.Fatalf("Role mismatch: got %q", got.Role)
	}
}

func TestMongoUserRepo_Create_DuplicatePhone(t *testing.T) {
	t.Parallel()

	database, colName := startMongoForTest(t)
	repo := repoFor(t, database, colName)

	base := &UserDoc{Phone: "13800138001", PasswordHash: "h1", Role: "rider"}
	if err := repo.Create(context.Background(), base); err != nil {
		t.Fatalf("first create: %v", err)
	}

	dup := &UserDoc{Phone: "13800138001", PasswordHash: "h2", Role: "rider"}
	err := repo.Create(context.Background(), dup)
	if !errors.Is(err, ErrPhoneTaken) {
		t.Fatalf("expected ErrPhoneTaken, got: %v", err)
	}
}

func TestMongoUserRepo_FindByPhone_NotFound(t *testing.T) {
	t.Parallel()

	database, colName := startMongoForTest(t)
	repo := repoFor(t, database, colName)

	_, err := repo.FindByPhone(context.Background(), "nonexistent-1")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got: %v", err)
	}
}

func TestMongoUserRepo_FindByUUID_Basic(t *testing.T) {
	t.Parallel()

	database, colName := startMongoForTest(t)
	repo := repoFor(t, database, colName)

	doc := &UserDoc{Phone: "13800138002", PasswordHash: "h", Role: "driver"}
	if err := repo.Create(context.Background(), doc); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.FindByUUID(context.Background(), doc.UUID)
	if err != nil {
		t.Fatalf("find by uuid: %v", err)
	}
	if got.Phone != "13800138002" {
		t.Fatalf("phone mismatch: %q", got.Phone)
	}
}

// TestMongoUserRepo_Create_IndexEnforced 模拟并发创建同一 phone。
// 预期：唯一索引在 DB 层拦住，至多 1 次成功，其余都返回 ErrPhoneTaken。
func TestMongoUserRepo_Create_IndexEnforced(t *testing.T) {
	t.Parallel()

	database, colName := startMongoForTest(t)
	repo := repoFor(t, database, colName)

	const N = 20
	phone := "13800138099"
	errs := make(chan error, N)
	var wg sync.WaitGroup
	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- repo.Create(context.Background(), &UserDoc{Phone: phone, PasswordHash: "h", Role: "rider"})
		}()
	}
	wg.Wait()
	close(errs)

	success := 0
	dup := 0
	for err := range errs {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrPhoneTaken):
			dup++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("expected exactly 1 success, got %d (dup=%d)", success, dup)
	}
	if dup != N-1 {
		t.Fatalf("expected %d duplicates, got %d", N-1, dup)
	}
}
