// Package main 是演唱会压测 § 3.Step 4 的种子数据生成器。
//
// 职责：通过 api-gateway 的 /auth/register 接口批量注册
//   - 5000 个 rider，手机号 13800000001 ~ 13800005000
//   - 1000 个 driver，手机号 13900000001 ~ 13900001000
//
// 最终把 (phone, uuid, role) 写到 loadtest/k6/seed/users.json，
// k6 脚本用 SharedArray 读它，所有 VU 共享同一份账号池。
//
// 为什么写成独立可执行 Go：
//   - 纯 HTTP 注册够用，不需要 mongo-go-driver 直接写库（方案 § 3.Step 4 明示走 HTTP）；
//   - 走 HTTP 能复用 api-gateway 的密码哈希 + UUID 生成，保证和真实登录流程一致；
//   - 独立 package main 方便 `go run loadtest/k6/seed/seed.go` 直接跑，不污染业务服务编译。
//
// 幂等性：二次运行时所有请求会返回 409 phone_taken，脚本把 409 视为 OK 不中断，
// 和方案 § 3.Step 4 的「重跑 <10s 全部 409」要求一致。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// SeedRecord 是 users.json 里每条账号的最小字段集。
// 只保留 phone / password / userID / role 四个字段，k6 脚本按需拼登录请求。
type SeedRecord struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
	UserID   string `json:"userID,omitempty"` // 409 幂等重跑时可能取不到，保持可空。
	Role     string `json:"role"`
}

// apiResponse 与 api-gateway contracts.APIResponse 同构。
// 这里不引 shared/contracts，避免 loadtest 目录强耦合业务包；仅解需要的字段。
type apiResponse struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type registerData struct {
	UserID string `json:"userID"`
}

const (
	defaultPassword = "password123" // 压测环境固定口令；严禁复用到生产。
	defaultRider    = 5000
	defaultDriver   = 1000
	riderPhoneBase  = "138" // 138 0000 0001 起
	driverPhoneBase = "139" // 139 0000 0001 起
)

func main() {
	gatewayURL := flag.String("gateway", envOr("GATEWAY_URL", "http://localhost:8081"), "api-gateway base URL")
	outputPath := flag.String("out", "loadtest/k6/seed/users.json", "output JSON file")
	riderCount := flag.Int("riders", defaultRider, "number of riders to register")
	driverCount := flag.Int("drivers", defaultDriver, "number of drivers to register")
	concurrency := flag.Int("concurrency", 50, "parallel HTTP workers")
	flag.Parse()

	if err := run(*gatewayURL, *outputPath, *riderCount, *driverCount, *concurrency); err != nil {
		log.Fatalf("seed failed: %v", err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// run 是主流程的纯函数版本，便于单测。
func run(gatewayURL, outputPath string, riders, drivers, concurrency int) error {
	start := time.Now()

	// 1. 构造任务队列：rider 在前，driver 在后，总量 riders+drivers。
	jobs := make([]SeedRecord, 0, riders+drivers)
	for i := 1; i <= riders; i++ {
		jobs = append(jobs, SeedRecord{
			Phone:    fmt.Sprintf("%s%08d", riderPhoneBase, i),
			Password: defaultPassword,
			Role:     "rider",
		})
	}
	for i := 1; i <= drivers; i++ {
		jobs = append(jobs, SeedRecord{
			Phone:    fmt.Sprintf("%s%08d", driverPhoneBase, i),
			Password: defaultPassword,
			Role:     "driver",
		})
	}

	// 2. 并发注册。worker 池固定 concurrency 个，避免把 api-gateway 打死。
	//    返回结果通过 index 原位回写，保持 jobs 顺序稳定，方便 k6 分片。
	results := make([]SeedRecord, len(jobs))
	ch := make(chan int, len(jobs))
	for i := range jobs {
		ch <- i
	}
	close(ch)

	var okCount, dupCount, failCount int64
	client := &http.Client{Timeout: 10 * time.Second}

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			for idx := range ch {
				rec, status, err := registerOne(client, gatewayURL, jobs[idx])
				switch {
				case err != nil:
					atomic.AddInt64(&failCount, 1)
					// 打日志但不中断整体：允许少量失败，最终会在 summary 提示。
					log.Printf("register %s failed: %v", jobs[idx].Phone, err)
					results[idx] = jobs[idx] // 保留请求信息（无 UUID）避免空洞
				case status == http.StatusCreated:
					atomic.AddInt64(&okCount, 1)
					results[idx] = rec
				case status == http.StatusConflict:
					// 幂等路径：手机号已注册，业务角度成功。UUID 不回填，k6 靠 phone+password 登录。
					atomic.AddInt64(&dupCount, 1)
					results[idx] = jobs[idx]
				default:
					atomic.AddInt64(&failCount, 1)
					log.Printf("register %s: unexpected status %d", jobs[idx].Phone, status)
					results[idx] = jobs[idx]
				}
			}
		}()
	}
	wg.Wait()

	// 3. 写 users.json。atomic rename 避免中途崩溃产出半写文件。
	if err := writeJSON(outputPath, results); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	log.Printf("done: ok=%d dup=%d fail=%d total=%d elapsed=%s",
		okCount, dupCount, failCount, len(jobs), time.Since(start))
	if failCount > 0 {
		return fmt.Errorf("%d registrations failed (see log above)", failCount)
	}
	return nil
}

// registerOne 发一次 POST /auth/register，返回带 userID 的 SeedRecord 和 HTTP 状态。
// 409 phone_taken 不视为 err，返回 (input, 409, nil)，交给上层按幂等处理。
func registerOne(client *http.Client, gatewayURL string, rec SeedRecord) (SeedRecord, int, error) {
	body, err := json.Marshal(map[string]string{
		"phone":    rec.Phone,
		"password": rec.Password,
		"role":     rec.Role,
	})
	if err != nil {
		return rec, 0, fmt.Errorf("marshal body: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/auth/register", bytes.NewReader(body))
	if err != nil {
		return rec, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return rec, 0, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	// 只在 201 情况下解析 UUID；其他情况直接返回原 rec。
	if resp.StatusCode != http.StatusCreated {
		return rec, resp.StatusCode, nil
	}

	var env apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return rec, resp.StatusCode, fmt.Errorf("decode response: %w", err)
	}
	if env.Error != nil {
		return rec, resp.StatusCode, fmt.Errorf("api error: %s - %s", env.Error.Code, env.Error.Message)
	}
	var data registerData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return rec, resp.StatusCode, fmt.Errorf("decode data: %w", err)
	}
	rec.UserID = data.UserID
	return rec, resp.StatusCode, nil
}

// writeJSON 把 records 以稳定格式写 path，先写临时文件再 rename 保证原子性。
func writeJSON(path string, records []SeedRecord) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(records); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("encode: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
