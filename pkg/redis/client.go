package redis

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client represents a Redis client with publish/subscribe capabilities
type Client struct {
	client *redis.Client
	ctx    context.Context
}

// New creates a new Redis client
func New(addr string, password string, db int) (*Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %v", err)
	}

	return &Client{
		client: client,
		ctx:    ctx,
	}, nil
}

// WriteString writes a string value to Redis
func (c *Client) WriteString(key, field, value string) error {
	return c.client.HSet(c.ctx, key, field, value).Err()
}

// WriteAndPublishString writes a string value to Redis and publishes it
func (c *Client) WriteAndPublishString(key, field, value string) error {
	pipe := c.client.Pipeline()
	pipe.HSet(c.ctx, key, field, value)
	pipe.Publish(c.ctx, key, fmt.Sprintf("%s:%s", field, value))
	_, err := pipe.Exec(c.ctx)
	return err
}

// WriteInt writes an integer value to Redis
func (c *Client) WriteInt(key, field string, value int) error {
	return c.client.HSet(c.ctx, key, field, value).Err()
}

// WriteAndPublishInt writes an integer value to Redis and publishes it
func (c *Client) WriteAndPublishInt(key, field string, value int) error {
	pipe := c.client.Pipeline()
	pipe.HSet(c.ctx, key, field, value)
	pipe.Publish(c.ctx, key, fmt.Sprintf("%s:%d", field, value))
	_, err := pipe.Exec(c.ctx)
	return err
}

// GetString gets a string value from Redis
func (c *Client) GetString(key, field string) (string, error) {
	val, err := c.client.HGet(c.ctx, key, field).Result()
	if err == redis.Nil {
		return "", fmt.Errorf("key %s field %s not found", key, field)
	}
	return val, err
}

// GetInt gets an integer value from Redis
func (c *Client) GetInt(key, field string) (int, error) {
	val, err := c.client.HGet(c.ctx, key, field).Result()
	if err == redis.Nil {
		return 0, fmt.Errorf("key %s field %s not found", key, field)
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(val)
}

// Subscribe subscribes to a Redis channel and returns a channel for messages
func (c *Client) Subscribe(channel string) (<-chan *redis.Message, func()) {
	pubsub := c.client.Subscribe(c.ctx, channel)
	ch := pubsub.Channel()
	return ch, func() { pubsub.Close() }
}

// Publish publishes a message to a Redis channel
func (c *Client) Publish(channel string, message string) error {
	return c.client.Publish(c.ctx, channel, message).Err()
}

// Close closes the Redis client connection
func (c *Client) Close() error {
	return c.client.Close()
}

// GetStateString gets a state value from Redis as a string
func (c *Client) GetStateString(key, field string) (string, error) {
	val, err := c.client.HGet(c.ctx, key, field).Result()
	if err == redis.Nil {
		return "", fmt.Errorf("key %s field %s not found", key, field)
	}
	return val, err
}

// GetStateInt gets a state value from Redis and converts it to an integer if possible
func (c *Client) GetStateInt(key, field string) (int, error) {
	val, err := c.GetStateString(key, field)
	if err != nil {
		return 0, err
	}

	// Try to convert string to integer
	switch val {
	case "standby":
		return 0, nil
	case "parked":
		return 1, nil
	case "ready-to-drive":
		return 2, nil
	case "shutting-down":
		return 3, nil
	case "updating":
		return 4, nil
	case "off":
		return 5, nil
	case "running":
		return 1, nil
	case "closed":
		return 0, nil
	case "open":
		return 1, nil
	default:
		// Try to parse as integer
		return strconv.Atoi(val)
	}
}

// HDel deletes a field from a hash in Redis
func (c *Client) HDel(key, field string) (int64, error) {
	return c.client.HDel(c.ctx, key, field).Result()
}

// LPush performs an LPUSH command on the specified list key.
func (c *Client) LPush(key string, value string) error {
	_, err := c.client.LPush(c.ctx, key, value).Result()
	if err != nil {
		log.Printf("Failed to LPUSH %s to key %s: %v", value, key, err)
		return err
	}
	return nil
}

// BRPop performs a blocking right pop (BRPOP) on a Redis list.
// It waits for 'timeout' seconds. If timeout is 0, it blocks indefinitely.
func (c *Client) BRPop(timeout time.Duration, key string) ([]string, error) {
	result, err := c.client.BRPop(c.ctx, timeout, key).Result()
	if err != nil {
		// redis.Nil indicates a timeout occurred, which is not necessarily an error in blocking operations
		if err == redis.Nil {
			return nil, nil // Return nil slice and nil error for timeout
		}
		log.Printf("Error during BRPOP on key %s: %v", key, err)
		return nil, err
	}
	// result is []string{key, value}
	if len(result) != 2 {
		log.Printf("Unexpected result length from BRPOP on key %s: %d", key, len(result))
		return nil, fmt.Errorf("unexpected result from BRPOP: %v", result)
	}
	return result, nil
}
