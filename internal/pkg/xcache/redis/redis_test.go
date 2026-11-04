package redis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	redis "github.com/redis/go-redis/v9"
)

// Test struct for JSON encoding/decoding.
type TestStruct struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

func TestRedisStoreSetAndGetWithStruct(t *testing.T) {
	// Given
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx := context.Background()

	client := NewMockRedisClientInterface(ctrl)

	testValue := TestStruct{Name: "test", Value: 123}
	redisStore := NewRedisStore[TestStruct](client)
	physicalKey := redisStore.valueKey("my-key")

	// Expect the client to receive the JSON-encoded value
	client.EXPECT().Set(ctx, physicalKey, "{\"name\":\"test\",\"value\":123}", time.Duration(0)).Return(&redis.StatusCmd{})
	client.EXPECT().Get(ctx, physicalKey).Return(redis.NewStringResult("{\"name\":\"test\",\"value\":123}", nil))

	// When
	RedisStore := redisStore
	err := RedisStore.Set(ctx, "my-key", testValue)
	assert.NoError(t, err)

	value, err := RedisStore.Get(ctx, "my-key")

	// Then
	assert.NoError(t, err)

	tv, ok := value.(TestStruct)
	assert.True(t, ok)
	assert.Equal(t, testValue.Name, tv.Name)
	assert.Equal(t, testValue.Value, tv.Value)
}

func TestRedisStoreGetWithTTL(t *testing.T) {
	// Given
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx := context.Background()

	client := NewMockRedisClientInterface(ctrl)

	testValue := TestStruct{Name: "test", Value: 123}
	ttlValue := 10 * time.Second
	redisStore := NewRedisStore[TestStruct](client)
	physicalKey := redisStore.valueKey("my-key")

	client.EXPECT().Get(ctx, physicalKey).Return(redis.NewStringResult("{\"name\":\"test\",\"value\":123}", nil))
	client.EXPECT().TTL(ctx, physicalKey).Return(redis.NewDurationResult(ttlValue, nil))

	// When
	RedisStore := redisStore
	value, ttl, err := RedisStore.GetWithTTL(ctx, "my-key")

	// Then
	assert.NoError(t, err)
	assert.Equal(t, ttlValue, ttl)

	tv, ok := value.(TestStruct)
	assert.True(t, ok)
	assert.Equal(t, testValue.Name, tv.Name)
	assert.Equal(t, testValue.Value, tv.Value)
}

func TestRedisStoreWithString(t *testing.T) {
	// Given
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx := context.Background()

	client := NewMockRedisClientInterface(ctrl)
	expectedValue := `test string`
	encodedValue := `"test string"`
	redisStore := NewRedisStore[string](client)
	physicalKey := redisStore.valueKey("my-key")

	client.EXPECT().Set(ctx, physicalKey, encodedValue, time.Duration(0)).Return(&redis.StatusCmd{})
	client.EXPECT().Get(ctx, physicalKey).Return(redis.NewStringResult(encodedValue, nil))

	// When
	RedisStore := redisStore
	err := RedisStore.Set(ctx, "my-key", expectedValue)
	assert.NoError(t, err)

	value, err := RedisStore.Get(ctx, "my-key")

	// Then
	assert.NoError(t, err)
	assert.Equal(t, expectedValue, value.(string))
}

func TestRedisStoreWithInt(t *testing.T) {
	// Given
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx := context.Background()

	client := NewMockRedisClientInterface(ctrl)
	testValue := 42
	redisStore := NewRedisStore[int](client)
	physicalKey := redisStore.valueKey("my-key")

	// When storing int, it's formatted as a string "42"
	client.EXPECT().Set(ctx, physicalKey, "42", time.Duration(0)).Return(&redis.StatusCmd{})
	// When retrieving, we try to unmarshal "42" which should work for int
	client.EXPECT().Get(ctx, physicalKey).Return(redis.NewStringResult("42", nil))

	// When
	RedisStore := redisStore
	err := RedisStore.Set(ctx, "my-key", testValue)
	assert.NoError(t, err)

	value, err := RedisStore.Get(ctx, "my-key")

	// Then
	assert.NoError(t, err)
	assert.Equal(t, testValue, value.(int))
}
