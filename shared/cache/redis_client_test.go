package cache

import (
	"os"
	"testing"
)

func TestNewRedisClientFromEnv_StandaloneFallback(t *testing.T) {
	t.Setenv("REDIS_SENTINEL_ADDRS", "")
	t.Setenv("REDIS_ADDR", "redis-dev:6379")
	t.Setenv("REDIS_DB", "2")

	result := NewRedisClientFromEnv()
	defer result.Client.Close()

	if result.Mode != RedisClientModeStandalone {
		t.Fatalf("expected standalone mode, got %s", result.Mode)
	}
	if result.EndpointHint != "redis-dev:6379" {
		t.Fatalf("unexpected endpoint hint: %s", result.EndpointHint)
	}
}

func TestNewRedisClientFromEnv_SentinelPreferred(t *testing.T) {
	t.Setenv("REDIS_SENTINEL_ADDRS", "  s1:26379, s2:26379 ,s3:26379 ")
	t.Setenv("REDIS_MASTER_NAME", "ride-master")
	t.Setenv("REDIS_ADDR", "redis-dev:6379")

	result := NewRedisClientFromEnv()
	defer result.Client.Close()

	if result.Mode != RedisClientModeSentinel {
		t.Fatalf("expected sentinel mode, got %s", result.Mode)
	}
	if result.EndpointHint != "s1:26379,s2:26379,s3:26379" {
		t.Fatalf("unexpected endpoint hint: %s", result.EndpointHint)
	}
}

func TestParseSentinelAddrs(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "empty", input: "", want: nil},
		{name: "spaces", input: "   ", want: nil},
		{name: "normal", input: "a:1,b:2", want: []string{"a:1", "b:2"}},
		{name: "with blanks", input: "a:1, , b:2,", want: []string{"a:1", "b:2"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSentinelAddrs(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("length mismatch, got=%v want=%v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("value mismatch, got=%v want=%v", got, tc.want)
				}
			}
		})
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}
