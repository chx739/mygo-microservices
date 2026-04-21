// 演唱会压测方案 § 3.Step 4 的种子脚本自测。
//
// 覆盖三条关键路径：
//   1. registerOne 对 201 响应能解析出 UUID；
//   2. registerOne 对 409 响应把 status 透传回来、不报错（幂等路径）；
//   3. run 端到端把 rider+driver 全部写进 users.json，文件内容可被再次解码。
//
// 不启真 Mongo / api-gateway，用 httptest.Server 替身 gateway，
// 符合方案 § 4 层级 1「纯单元」的定位。

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRegisterOne_201(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/register" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"phone":"13800000001"`) {
			t.Fatalf("request body missing phone: %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"userID":"uuid-abc"}}`))
	}))
	defer server.Close()

	client := server.Client()
	rec := SeedRecord{Phone: "13800000001", Password: "password123", Role: "rider"}
	got, status, err := registerOne(client, server.URL, rec)
	if err != nil {
		t.Fatalf("registerOne returned err: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("status got %d want 201", status)
	}
	if got.UserID != "uuid-abc" {
		t.Fatalf("UserID got %q want %q", got.UserID, "uuid-abc")
	}
}

func TestRegisterOne_Idempotent409(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"phone_taken","message":"phone already registered"}}`))
	}))
	defer server.Close()

	client := server.Client()
	rec := SeedRecord{Phone: "13800000002", Password: "password123", Role: "rider"}
	_, status, err := registerOne(client, server.URL, rec)
	if err != nil {
		t.Fatalf("409 must not be treated as error, got: %v", err)
	}
	if status != http.StatusConflict {
		t.Fatalf("status got %d want 409", status)
	}
}

func TestRun_WritesUsersJSON(t *testing.T) {
	// 混合 201 + 409：确保两条路径都进 users.json。
	var calls int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 偶数请求返回 409，奇数返回 201，模拟 50% 幂等重跑场景。
		n := atomic.AddInt64(&calls, 1)
		if n%2 == 0 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"userID":"uid-x"}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "users.json")

	// riders=3 drivers=2，总共 5 条记录，足够覆盖 role 字段分布。
	if err := run(server.URL, out, 3, 2, 4); err != nil {
		t.Fatalf("run err: %v", err)
	}

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var records []SeedRecord
	if err := json.Unmarshal(b, &records); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(records) != 5 {
		t.Fatalf("records count got %d want 5", len(records))
	}

	// 前 3 条 rider，后 2 条 driver，顺序稳定。
	wantRoles := []string{"rider", "rider", "rider", "driver", "driver"}
	for i, r := range records {
		if r.Role != wantRoles[i] {
			t.Fatalf("record[%d].Role got %q want %q", i, r.Role, wantRoles[i])
		}
		if r.Phone == "" || r.Password == "" {
			t.Fatalf("record[%d] missing phone/password: %+v", i, r)
		}
	}
}
