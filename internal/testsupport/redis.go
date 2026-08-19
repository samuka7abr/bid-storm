package testsupport

import (
	"context"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/samuka7abr/bid-storm/internal/db"
)

// redis:7-alpine, the image of the compose. A middleware that passed against a
// different Redis than the one the benchmark runs would be proving something
// about a build nobody measures.
const redisImage = "redis:7-alpine"

// Redis is the container shared by every test in the package that asks for it.
type Redis struct {
	Client *redis.Client
	URL    string
}

var (
	redisOnce sync.Once
	sharedRDB *Redis
	redisErr  error
)

// StartRedis returns the package-wide Redis, booting it on the first call.
func StartRedis(t *testing.T) *Redis {
	t.Helper()
	redisOnce.Do(func() { sharedRDB, redisErr = startRedis() })
	if redisErr != nil {
		t.Fatalf("start redis: %v", redisErr)
	}
	return sharedRDB
}

func startRedis() (*Redis, error) {
	ctx := context.Background()

	container, err := tcredis.Run(ctx, redisImage)
	if err != nil {
		return nil, err
	}

	url, err := container.ConnectionString(ctx)
	if err != nil {
		return nil, err
	}

	// Through the same constructor the process uses, so a URL this helper
	// accepts is a URL auctiond accepts.
	client, err := db.NewRedis(ctx, url)
	if err != nil {
		return nil, err
	}
	return &Redis{Client: client, URL: url}, nil
}
