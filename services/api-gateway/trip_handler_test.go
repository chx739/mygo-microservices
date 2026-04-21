// 演唱会压测方案 § 3.Step 4.5 的 GET /trip/:id 透传 handler 单测。
//
// 单元层面只覆盖「不触达下游 gRPC」的分支 —— 空 id 的 400。
// 为什么不 mock trip gRPC client：
//   - 现有 handleTripPreview / handleTripStart 也没有单测，它们走
//     grpc_clients.NewTripServiceClient() 直调，改为注入式会扩散改动到
//     其他 handler，违反方案「不改业务代码」的原则；
//   - 200 / 404 真实链路在 tilt up 后的 E2E curl 验证（方案 § 3.Step 4.5）
//     + k6 attackerFlow 压测中实际触达，不在本单测里覆盖。

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleGetTrip_EmptyID_400(t *testing.T) {
	// 构造一个没有 {id} 路径值的请求。ServeMux 的 PathValue 返回 ""，
	// handler 应直接返回 400，不尝试连接 trip-service。
	req := httptest.NewRequest(http.MethodGet, "/trip/", nil)
	rec := httptest.NewRecorder()

	handleGetTrip(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status got %d want 400", rec.Code)
	}
}
