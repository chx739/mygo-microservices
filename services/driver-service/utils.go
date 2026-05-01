package main

import "math/rand"

// Predefined routes for drivers (used for the gRPC Streaming module).
// 演唱会压测方案的虚拟场馆是广州天河体育中心 (23.1291°N, 113.2644°E)，
// rider 流量集中在场馆 2km 内（loadtest/k6/scenarios/helpers/payload.js
// VENUE_LAT/LON），driver-service 默认 GEO 搜索半径 3km
// (defaultGeoSearchRadiusKm, services/driver-service/service.go:30)。
// Round 2 重跑前这里写死的是 SF 旧金山坐标，rider/driver 距离 ~12000km，
// FindNearbyDrivers 永远 0 个候选，导致 trip_assigned_within_15s 永 0%。
// 现在替换成场馆周围 4 条 ~1km 内的环线，保证压测时 GEO 命中。
// 注意：service_test.go 自己 hardcode 测试坐标，不依赖此处。
var PredefinedRoutes = [][][]float64{
	// 场馆东侧（天河北路一带）
	{
		{23.1305, 113.2660},
		{23.1310, 113.2670},
		{23.1308, 113.2680},
		{23.1300, 113.2675},
	},
	// 场馆北侧（体育西路 - 天河路）
	{
		{23.1298, 113.2640},
		{23.1305, 113.2635},
		{23.1312, 113.2640},
		{23.1318, 113.2650},
		{23.1320, 113.2660},
		{23.1315, 113.2670},
	},
	// 场馆西南（体育中心南门 - 黄埔大道）
	{
		{23.1280, 113.2625},
		{23.1275, 113.2635},
		{23.1270, 113.2645},
		{23.1278, 113.2655},
		{23.1285, 113.2650},
	},
	// 场馆正南（黄埔大道 - 体育东路）
	{
		{23.1275, 113.2660},
		{23.1278, 113.2670},
		{23.1282, 113.2680},
		{23.1285, 113.2685},
		{23.1290, 113.2680},
	},
}

func GenerateRandomPlate() string {
	letters := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	plate := ""
	for i := 0; i < 3; i++ {
		plate += string(letters[rand.Intn(len(letters))])
	}

	return plate
}
